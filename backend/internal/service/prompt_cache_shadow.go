package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/promptcache"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type promptCacheShadowState struct {
	once   sync.Once
	engine *promptcache.Engine
	// Diagnostic work must not accumulate behind concurrent model requests.
	workers              chan struct{}
	log                  *promptcache.ObserverLog
	closed               atomic.Bool
	stopState, stateDone chan struct{}
	stateRestored        bool
	closeOnce            sync.Once
	savedStateRevision   uint64
	stateSaved           bool
	// Emit is replaceable by tests; production uses the independent file worker.
	emit func([]zap.Field)
}

func (s *promptCacheShadowState) get(cfg *config.Config, account *Account) *promptcache.Engine {
	if cfg == nil || account == nil || account.Platform != PlatformOpenAI || cfg.IsSecondary() || !cfg.Gateway.PromptCacheShadow.Enabled || s.closed.Load() {
		return nil
	}
	settings := cfg.Gateway.PromptCacheShadow
	if len(settings.AccountIDs) > 0 {
		selected := false
		for _, id := range settings.AccountIDs {
			selected = selected || id == account.ID
		}
		if !selected {
			return nil
		}
	}
	s.init(cfg)
	return s.engine
}

func (s *promptCacheShadowState) init(cfg *config.Config) {
	if cfg == nil || cfg.IsSecondary() || !cfg.Gateway.PromptCacheShadow.Enabled {
		return
	}
	s.once.Do(func() {
		if s.closed.Load() {
			return
		}
		settings := cfg.Gateway.PromptCacheShadow
		concurrency := settings.ComputeConcurrency
		if concurrency <= 0 {
			concurrency = 8
		}
		s.workers = make(chan struct{}, concurrency)
		var err error
		s.engine, err = promptcache.New(promptcache.Options{TTL: time.Duration(settings.TTLSeconds) * time.Second, MinPrefixTokens: settings.MinPrefixTokens, PrefixTokensStep: settings.PrefixTokensStep, MaxEntries: settings.MaxEntries, MaxBodyBytes: settings.MaxBodyBytes})
		if err != nil {
			logger.L().Warn("prompt_cache.shadow_disabled", zap.String("reason", "random_source_unavailable"))
			return
		}
		if s.emit != nil {
			return
		}
		dir := settings.LogDir
		if dir == "" {
			dataDir := os.Getenv("DATA_DIR")
			if dataDir == "" {
				dataDir = "/app/data"
			}
			dir = filepath.Join(dataDir, "logs", "prompt-cache-shadow")
		}
		s.log, err = promptcache.NewObserverLog(promptcache.LogOptions{
			Dir: dir, InstanceID: s.engine.InstanceID(), Retention: time.Duration(settings.LogRetentionDays) * 24 * time.Hour,
			MaxTotalBytes: int64(settings.LogMaxTotalMB) << 20, QueueSize: settings.LogQueueSize,
			Warn: func(err error) {
				logger.L().Warn("prompt_cache.shadow_log_error", zap.Error(err), zap.Bool(logger.OpsSystemLogSkipField, true))
			},
		})
		if err != nil {
			s.engine = nil
			logger.L().Warn("prompt_cache.shadow_disabled", zap.String("reason", "log_initialization_failed"), zap.Error(err), zap.Bool(logger.OpsSystemLogSkipField, true))
			return
		}
		s.startState(dir)
	})
}

func (s *promptCacheShadowState) close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.once.Do(func() {})
		if s.stopState != nil {
			close(s.stopState)
			<-s.stateDone
		}
		if s.log != nil {
			s.log.Close()
		}
	})
}

func (s *promptCacheShadowState) startState(logDir string) {
	dir := filepath.Join(logDir, "state")
	warn := func(err error) {
		logger.L().Warn("prompt_cache.shadow_state_error", zap.Error(err), zap.Bool(logger.OpsSystemLogSkipField, true))
	}
	root, err := os.OpenRoot(logDir)
	if err != nil {
		warn(err)
		return
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("state", 0700); err != nil && !os.IsExist(err) {
		warn(err)
		return
	}
	info, err := root.Lstat("state")
	if err != nil {
		warn(err)
		return
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		warn(fmt.Errorf("cache state directory is not private"))
		return
	}
	path := filepath.Join(dir, "cache.bin")
	err = s.engine.Load(path, time.Now())
	if err == nil {
		s.stateRestored = true
	} else if !os.IsNotExist(err) {
		warn(err)
	}
	s.stopState, s.stateDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.stateDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopState:
				if err := s.saveState(path); err != nil {
					warn(err)
				}
				return
			case <-ticker.C:
				if err := s.saveState(path); err != nil {
					warn(err)
				}
			}
		}
	}()
}

// Only the state worker calls this, including its final shutdown save. Reads
// and disabled channels do not force full snapshot copies and fsyncs.
func (s *promptCacheShadowState) saveState(path string) error {
	revision := s.engine.StateRevision()
	if s.stateSaved && revision == s.savedStateRevision {
		return nil
	}
	if err := s.engine.Save(path, time.Now()); err != nil {
		return err
	}
	s.savedStateRevision, s.stateSaved = revision, true
	return nil
}

type promptCacheShadowContextKey struct{}
type promptCacheShadowAttempt struct {
	mu                  sync.Mutex
	state               *promptCacheShadowState
	engine              *promptcache.Engine
	scope               promptcache.Scope
	apiKeyID, groupID   int64
	upstreamChannelID   int64
	model, platform     string
	transport, bodyView string
	attempt             *promptcache.Attempt
	captures            int
	finished            bool
	nativeCacheKnown    bool
	nativeCacheTokens   int
}

func (s *OpenAIGatewayService) beginPromptCacheShadow(ctx context.Context, c *gin.Context, account *Account) (context.Context, *promptCacheShadowAttempt) {
	if s == nil {
		return ctx, nil
	}
	prior, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	e := s.promptCacheShadow.get(s.cfg, account)
	var upstreamChannelID int64
	if e != nil {
		upstreamChannelID = s.channelService.PromptCacheShadowChannelForAccount(ctx, account.ID)
	}
	if e == nil || upstreamChannelID == 0 {
		// A failover to an unobserved account must not capture through an old
		// request context retained by metering or encrypted-history recovery.
		if prior != nil {
			ctx = context.WithValue(ctx, promptCacheShadowContextKey{}, (*promptCacheShadowAttempt)(nil))
		}
		return ctx, nil
	}
	if prior != nil {
		prior.mu.Lock()
		reuse := !prior.finished && prior.state == &s.promptCacheShadow && prior.scope.AccountID == account.ID && prior.upstreamChannelID == upstreamChannelID
		prior.mu.Unlock()
		if reuse {
			return ctx, nil
		}
	}
	a := newPromptCacheShadowAttempt(&s.promptCacheShadow, e, c, account)
	a.upstreamChannelID = upstreamChannelID
	return context.WithValue(ctx, promptCacheShadowContextKey{}, a), a
}

func newPromptCacheShadowAttempt(state *promptCacheShadowState, e *promptcache.Engine, c *gin.Context, account *Account) *promptCacheShadowAttempt {
	a := &promptCacheShadowAttempt{state: state, engine: e, apiKeyID: getAPIKeyIDFromContext(c), groupID: getOpenAIGroupIDFromContext(c), platform: account.Platform, model: "unknown", transport: "unknown", bodyView: "unavailable"}
	a.scope = promptcache.Scope{Tenant: fmt.Sprintf("key:%d/group:%d", a.apiKeyID, a.groupID), Platform: account.Platform, AccountID: account.ID}
	if c != nil {
		if key := getAPIKeyFromContext(c); key != nil && key.UserID > 0 {
			a.scope.Tenant = fmt.Sprintf("user:%d", key.UserID)
		}
	}
	return a
}

// capturePromptCacheShadow runs at the actual send boundary, after protocol,
// tool, model and identity rewrites. The request body and token ids are discarded
// immediately after fingerprinting; none reach logs or the retained cache.
func capturePromptCacheShadow(ctx context.Context, body []byte, transport string) {
	a, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a == nil {
		return
	}
	a.capture(body, transport, "upstream_wire")
}

func capturePromptCacheShadowValue(ctx context.Context, value any, routingHeaders ...http.Header) {
	a, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a == nil {
		return
	}
	var headers http.Header
	if len(routingHeaders) > 0 {
		headers = routingHeaders[0]
	}
	if raw, ok := value.(openAIWSPreparedJSON); ok {
		a.captureWire(raw, "ws", "upstream_wire", promptcache.ProtocolResponses, headers)
		return
	}
	a.capturePrepared("ws", "upstream_wire", func() ([]byte, error) {
		a.setWireScope(promptcache.ProtocolResponses, headers)
		return json.Marshal(value)
	})
}

func (a *promptCacheShadowAttempt) capture(body []byte, transport, view string) {
	a.captureWire(body, transport, view, "", nil)
}

func (a *promptCacheShadowAttempt) captureWire(body []byte, transport, view, protocol string, headers http.Header) {
	a.capturePrepared(transport, view, func() ([]byte, error) { a.setWireScope(protocol, headers); return body, nil })
}

// Called only from the admitted loader while the attempt mutex is held.
func (a *promptCacheShadowAttempt) setWireScope(protocol string, headers http.Header) {
	a.scope.Protocol = protocol
	a.scope.RoutingFingerprint = a.engine.RoutingFingerprint(headers.Get("session_id"), headers.Get("conversation_id"))
}

func promptCacheShadowWireProtocol(path string) string {
	path = strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return promptcache.ProtocolChatCompletions
	case strings.HasSuffix(path, "/messages"):
		return promptcache.ProtocolMessages
	case strings.HasSuffix(path, "/responses"), strings.HasSuffix(path, "/responses/compact"):
		return promptcache.ProtocolResponses
	default:
		return ""
	}
}

// Admission covers body copies and serialization as well as tokenization. A
// busy observer must not copy hundreds of large concurrent request bodies.
func (a *promptCacheShadowAttempt) capturePrepared(transport, view string, load func() ([]byte, error)) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.scope.Model, a.model = "", "unknown"
	a.scope.Protocol, a.scope.RoutingFingerprint = "", ""
	a.transport = transport
	a.bodyView = view
	a.captures++
	a.nativeCacheKnown = false
	a.nativeCacheTokens = 0
	a.attempt.Release()
	select {
	case a.state.workers <- struct{}{}:
		defer func() { <-a.state.workers }()
	default:
		a.attempt = &promptcache.Attempt{Decision: "skipped", SkipReason: "observer_busy"}
		return
	}
	body, err := load()
	if err != nil {
		a.attempt = &promptcache.Attempt{Decision: "skipped", SkipReason: "wire_body_read_error"}
		return
	}
	if len(body) > a.engine.MaxBodyBytes() {
		a.attempt = &promptcache.Attempt{Decision: "skipped", SkipReason: "body_limit"}
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if model != "" {
		a.scope.Model, a.model = model, model
	}
	a.attempt = a.engine.Begin(a.scope, body, func(text string) ([]uint, error) {
		codec, err := openAIInputTokensCodecForModel(model)
		if err != nil {
			return nil, err
		}
		var ids []uint
		// Fixed UTF-8-safe chunks bound pathological single-token BPE work and
		// temporary piece arrays. Chunk boundaries are an explicit approximation.
		for len(text) > 0 {
			end := min(len(text), 8192)
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end--
			}
			part, _, err := codec.Encode(text[:end])
			if err != nil {
				return nil, err
			}
			ids = append(ids, part...)
			text = text[end:]
		}
		return ids, nil
	}, time.Now())
}

func capturePromptCacheShadowHTTP(request *http.Request) {
	if request == nil {
		return
	}
	a, _ := request.Context().Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a == nil || request.Method != http.MethodPost {
		return
	}
	if request.GetBody == nil {
		return
	}
	a.capturePrepared("http", "upstream_wire", func() ([]byte, error) {
		protocol := ""
		if request.URL != nil {
			protocol = promptCacheShadowWireProtocol(request.URL.Path)
		}
		a.setWireScope(protocol, request.Header)
		reader, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = reader.Close() }()
		// Reads an independent replay reader, never the body sent upstream.
		return io.ReadAll(io.LimitReader(reader, int64(a.engine.MaxBodyBytes())+1))
	})
}

func (a *promptCacheShadowAttempt) finish(result *OpenAIForwardResult, forwardErr error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	defer a.attempt.Release()
	settings := a.engine.Options()
	accountCapacity, sharedCapacity, warmCapacity := a.engine.CacheCapacities()
	anchorCapacity, opaqueCapacity := a.engine.AnchorCapacities()
	fields := []zap.Field{
		zap.String("component", "service.prompt_cache_shadow"),
		zap.Bool(logger.OpsSystemLogSkipField, true),
		zap.Int("schema_version", 3),
		zap.String("prediction_basis", "local_prefix_anchors_v3"),
		zap.String("mode", "observe_only"),
		zap.String("instance_id", a.engine.InstanceID()),
		zap.String("state_scope", "process"),
		zap.Bool("state_persistent", a.state.stopState != nil),
		zap.Bool("state_restored", a.state.stateRestored),
		zap.Int64("account_id", a.scope.AccountID),
		zap.Int64("upstream_channel_id", a.upstreamChannelID),
		zap.Int64("api_key_id", a.apiKeyID),
		zap.Int64("group_id", a.groupID),
		zap.String("platform", a.platform),
		zap.String("model", a.model),
		zap.String("transport", a.transport),
		zap.String("body_view", a.bodyView),
		zap.Int("captures", a.captures),
		zap.Int("ttl_seconds", int(settings.TTL/time.Second)),
		zap.Int("min_prefix_tokens", settings.MinPrefixTokens),
		zap.Int("prefix_tokens_step", settings.PrefixTokensStep),
		zap.String("tokenization_mode", "chunked_8192_bytes"),
		zap.String("warm_scope", "user_model"),
		zap.Int("account_prefix_capacity", accountCapacity),
		zap.Int("shared_prefix_capacity", sharedCapacity),
		zap.Int("warm_state_capacity", warmCapacity),
		zap.Int("measured_anchor_capacity", anchorCapacity),
		zap.Int("opaque_history_capacity", opaqueCapacity),
	}
	reason := ""
	comparable := true
	if a.attempt == nil {
		reason = "wire_body_unavailable"
		comparable = false
	}
	if a.attempt != nil && a.attempt.SkipReason != "" {
		reason = a.attempt.SkipReason
		comparable = false
	}
	input, cached, known := 0, 0, false
	if result == nil || result.UsageUnavailable {
		if reason == "" {
			reason = "upstream_usage_unavailable"
		}
		comparable = false
	} else {
		input = result.Usage.InputTokens
		cached = result.Usage.CacheReadInputTokens
		known = result.Usage.CacheReadKnown || (a.nativeCacheKnown && a.nativeCacheTokens == cached)
		fields = append(fields, zap.String("request_id", result.RequestID), zap.String("response_model", result.UpstreamResponseModel))
		if input <= 0 || cached < 0 || cached > input {
			if reason == "" {
				reason = "invalid_upstream_usage"
			}
			comparable = false
		}
		if !known {
			if reason == "" {
				reason = "upstream_cache_unknown"
			}
			comparable = false
		}
	}
	complete := forwardErr == nil && result != nil && !result.ClientDisconnect && result.SucceededForScheduling() && (result.UpstreamTerminalEvent == "" || result.UpstreamTerminalEvent == "response.completed" || result.UpstreamTerminalEvent == "response.done")
	if !complete {
		if reason == "" {
			reason = "incomplete_request"
		}
		comparable = false
	}
	fields = append(fields, zap.Bool("upstream_cache_known", known), zap.Bool("comparable", comparable), zap.String("skip_reason", reason))
	if result != nil && !result.UsageUnavailable {
		fields = append(fields, zap.Int("input_tokens", input))
	} else {
		fields = append(fields, zap.Any("input_tokens", nil))
	}
	if known {
		fields = append(fields, zap.Int("upstream_cache_read_tokens", cached))
	} else {
		fields = append(fields, zap.Any("upstream_cache_read_tokens", nil))
	}
	committed := false
	if a.attempt != nil {
		p := a.attempt
		fields = append(fields,
			zap.Bool("output_prefix_indexed", p.OutputIndexed),
			zap.Int("opaque_blocks", p.OpaqueBlocks),
			zap.Int("opaque_known_blocks", p.OpaqueKnownBlocks),
			zap.Int("opaque_output_known_blocks", p.OpaqueOutputKnownBlocks),
			zap.Int("opaque_unseen_blocks", p.OpaqueBlocks-p.OpaqueKnownBlocks),
			zap.String("measured_anchor_status", p.AnchorStatus),
			zap.Int64("measured_anchor_age_ms", p.MeasuredAnchorAge.Milliseconds()),
			zap.Uint64("opaque_history_evictions", p.OpaqueHistoryEvictions),
			zap.Uint64("measured_anchor_evictions", p.AnchorEvictions),
			zap.Uint64("measured_anchor_live_evictions", p.AnchorLiveEvictions),
			zap.Uint64("measured_anchor_expired_reclaims", p.AnchorExpiredReclaims),
			zap.Int("measured_prefix_input_tokens", p.MatchedInputTokens),
			zap.String("logical_scope", "user_platform_model_protocol"),
			zap.Uint64("account_live_evictions", p.AccountLiveEvictions),
			zap.Uint64("account_expired_reclaims", p.AccountExpiredReclaims),
			zap.String("decision", p.Decision),
			zap.String("prompt_layout", p.Layout),
			zap.String("protocol", p.Protocol),
			zap.Int("estimated_input_tokens", p.EstimatedInputTokens),
			zap.Int("matched_prefix_tokens", p.MatchedPrefixTokens),
			zap.Int("shared_prefix_tokens", p.SharedPrefixTokens),
			zap.String("prefix_fingerprint", p.PrefixFingerprint),
			zap.String("routing_fingerprint", p.RoutingFingerprint),
			zap.Uint64("account_cache_evictions", p.AccountEvictions),
			zap.Uint64("shared_cache_evictions", p.SharedEvictions),
			zap.Uint64("warm_state_evictions", p.WarmStateEvictions),
			zap.String("tokenizer", string(openAIInputTokensEncodingForModel(a.model))),
			zap.Int64("cache_age_ms", p.CacheAge.Milliseconds()),
			zap.Bool("expired", p.Expired),
			zap.Int64("duration_us", p.ComputedIn.Microseconds()),
		)
		if p.SkipReason == "" && input > 0 {
			predicted, raw := p.Compare(input)
			fields = append(fields,
				zap.Int("simulated_cache_read_tokens", predicted),
				zap.Int("raw_simulated_cache_read_tokens", raw),
				// Retain the old normalized-diagnostic field as an explicit alias.
				zap.Int("scaled_simulated_cache_read_tokens", predicted),
			)
			if known {
				fields = append(fields, zap.Int("cache_read_error_tokens", predicted-cached), zap.Float64("upstream_cache_hit_rate", float64(cached)/float64(input)), zap.Float64("simulated_cache_hit_rate", float64(predicted)/float64(input)))
			}
		} else {
			fields = append(fields, zap.Any("simulated_cache_read_tokens", nil))
		}
		if complete && !result.UsageUnavailable && input > 0 && cached >= 0 && cached <= input {
			committed = p.CommitUsage(input, max(0, result.Usage.OutputTokens), time.Now())
		}
	} else {
		fields = append(fields, zap.String("decision", "skipped"), zap.Any("simulated_cache_read_tokens", nil))
	}
	fields = append(fields, zap.Bool("committed", committed))
	if a.state.emit != nil {
		a.state.emit(fields)
	} else if a.state.log != nil {
		a.state.log.Emit(fields)
	}
}

func (s *OpenAIGatewayService) wrapPromptCacheShadowHooks(c *gin.Context, account *Account, hooks *OpenAIWSIngressHooks) (*OpenAIWSIngressHooks, func()) {
	e := s.promptCacheShadow.get(s.cfg, account)
	if e == nil {
		return hooks, func() {}
	}
	wrapped := OpenAIWSIngressHooks{}
	if hooks != nil {
		wrapped = *hooks
	}
	var mu sync.Mutex
	var active *promptCacheShadowAttempt
	activeTurn := -1
	priorObserve, priorAfter := wrapped.ObserveUpstreamRequest, wrapped.AfterTurn
	priorOutput := wrapped.ObserveUpstreamResponse
	wrapped.ObserveUpstreamResponse = func(turn int, payload []byte) {
		if priorOutput != nil {
			priorOutput(turn, payload)
		}
		mu.Lock()
		a := active
		current := activeTurn
		mu.Unlock()
		if a != nil && turn == current {
			a.captureOutput(string(payload))
		}
	}
	wrapped.ObserveUpstreamRequest = func(turn int, body []byte, routingHeaders ...http.Header) {
		if priorObserve != nil {
			priorObserve(turn, body, routingHeaders...)
		}
		mu.Lock()
		defer mu.Unlock()
		if turn != activeTurn {
			// A new frame can replace a turn before its terminal callback arrives.
			// Finish the old owner before losing its in-flight fingerprints.
			active.finish(nil, context.Canceled)
			active = nil
			activeTurn = turn
			ctx := context.Background()
			if c != nil && c.Request != nil {
				ctx = c.Request.Context()
			}
			if channelID := s.channelService.PromptCacheShadowChannelForAccount(ctx, account.ID); channelID > 0 {
				active = newPromptCacheShadowAttempt(&s.promptCacheShadow, e, c, account)
				active.upstreamChannelID = channelID
			}
		}
		if active == nil {
			return
		}
		var headers http.Header
		if len(routingHeaders) > 0 {
			headers = routingHeaders[0]
		}
		active.captureWire(body, "ws", "upstream_wire", promptcache.ProtocolResponses, headers)
	}
	wrapped.AfterTurn = func(turn int, result *OpenAIForwardResult, err error) {
		mu.Lock()
		a := active
		if turn == activeTurn {
			active = nil
		} else {
			a = nil
		}
		mu.Unlock()
		a.finish(result, err)
		if priorAfter != nil {
			priorAfter(turn, result, err)
		}
	}
	return &wrapped, func() { mu.Lock(); a := active; active = nil; mu.Unlock(); a.finish(nil, context.Canceled) }
}

// Keep diagnostic presence evidence separate from CacheReadKnown, which also
// controls existing anti-distillation enforcement. Observing must not enable a
// new ban/billing/scheduling behavior for native Anthropic compatibility routes.
func capturePromptCacheShadowAnthropicUsage(ctx context.Context, payload string) {
	a, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a == nil {
		return
	}
	root := gjson.Parse(payload)
	usage := root.Get("usage")
	if root.Get("type").String() == "message_start" {
		usage = root.Get("message.usage")
	}
	if !usage.IsObject() {
		return
	}
	fields := []string{"input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_tokens", "prompt_cache_hit_tokens"}
	present := false
	for _, path := range fields {
		present = present || usage.Get(path).Exists()
	}
	if !present {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished || !openAICacheReadKnownFromUsage(usage) {
		return
	}
	tokens := openAICacheReadTokensFromUsage(usage)
	// Match mergeAnthropicUsage's cumulative semantics: late empty/zero
	// snapshots cannot erase a previously confirmed positive cache bucket.
	if !a.nativeCacheKnown || tokens > 0 || a.nativeCacheTokens <= 0 {
		a.nativeCacheKnown, a.nativeCacheTokens = true, tokens
	}
}

func capturePromptCacheShadowResponseUsage(resp *http.Response, payload string) {
	if resp != nil && resp.Request != nil {
		capturePromptCacheShadowAnthropicUsage(resp.Request.Context(), payload)
	}
}

func capturePromptCacheShadowOutput(ctx context.Context, payload string) {
	a, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a == nil {
		return
	}
	a.captureOutput(payload)
}

func capturePromptCacheShadowOutputBytes(ctx context.Context, payload []byte) {
	a, _ := ctx.Value(promptCacheShadowContextKey{}).(*promptCacheShadowAttempt)
	if a != nil {
		a.captureOutput(string(payload))
	}
}

func capturePromptCacheShadowHTTPOutput(resp *http.Response, payload []byte) {
	if resp != nil && resp.Request != nil {
		capturePromptCacheShadowOutputBytes(resp.Request.Context(), payload)
	}
}

func (a *promptCacheShadowAttempt) captureOutput(payload string) {
	if len(payload) > a.engine.MaxBodyBytes() {
		return
	}
	root := gjson.Parse(payload)
	output := root.Get("output")
	if root.Get("type").String() == "response.completed" || root.Get("type").String() == "response.done" || root.Get("response.status").String() == "completed" {
		output = root.Get("response.output")
	}
	if !output.IsArray() || len(output.Raw) > a.engine.MaxBodyBytes() {
		return
	}
	select {
	case a.state.workers <- struct{}{}:
		defer func() { <-a.state.workers }()
	default:
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.finished && a.attempt != nil {
		a.attempt.ObserveOutput([]byte(output.Raw))
	}
}
