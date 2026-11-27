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
	workers chan struct{}
	log     *promptcache.ObserverLog
	closed  atomic.Bool
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
		}
	})
}

func (s *promptCacheShadowState) close() {
	s.closed.Store(true)
	s.once.Do(func() {})
	if s.log != nil {
		s.log.Close()
	}
}

type promptCacheShadowContextKey struct{}
type promptCacheShadowAttempt struct {
	mu                  sync.Mutex
	state               *promptCacheShadowState
	engine              *promptcache.Engine
	scope               promptcache.Scope
	apiKeyID, groupID   int64
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
	if e == nil {
		// A failover to an unobserved account must not capture through an old
		// request context retained by metering or encrypted-history recovery.
		if prior != nil {
			ctx = context.WithValue(ctx, promptCacheShadowContextKey{}, (*promptCacheShadowAttempt)(nil))
		}
		return ctx, nil
	}
	if prior != nil {
		prior.mu.Lock()
		reuse := !prior.finished && prior.state == &s.promptCacheShadow && prior.scope.AccountID == account.ID
		prior.mu.Unlock()
		if reuse {
			return ctx, nil
		}
	}
	a := newPromptCacheShadowAttempt(&s.promptCacheShadow, e, c, account)
	return context.WithValue(ctx, promptCacheShadowContextKey{}, a), a
}

func newPromptCacheShadowAttempt(state *promptCacheShadowState, e *promptcache.Engine, c *gin.Context, account *Account) *promptCacheShadowAttempt {
	a := &promptCacheShadowAttempt{state: state, engine: e, apiKeyID: getAPIKeyIDFromContext(c), groupID: getOpenAIGroupIDFromContext(c), platform: account.Platform, model: "unknown", transport: "unknown", bodyView: "unavailable"}
	a.scope = promptcache.Scope{Tenant: fmt.Sprintf("key:%d/group:%d", a.apiKeyID, a.groupID), Platform: account.Platform, AccountID: account.ID}
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
	fields := []zap.Field{
		zap.String("component", "service.prompt_cache_shadow"),
		zap.Bool(logger.OpsSystemLogSkipField, true),
		zap.Int("schema_version", 1),
		zap.String("mode", "observe_only"),
		zap.String("instance_id", a.engine.InstanceID()),
		zap.String("state_scope", "process"),
		zap.Int64("account_id", a.scope.AccountID),
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
		zap.Bool("output_prefix_indexed", false),
		zap.String("tokenization_mode", "chunked_8192_bytes"),
		zap.String("warm_scope", "account_model"),
		zap.Int("account_prefix_capacity", accountCapacity),
		zap.Int("shared_prefix_capacity", sharedCapacity),
		zap.Int("warm_state_capacity", warmCapacity),
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
			predicted, scaled := p.Compare(input)
			fields = append(fields, zap.Int("simulated_cache_read_tokens", predicted), zap.Int("scaled_simulated_cache_read_tokens", scaled))
			if known {
				fields = append(fields, zap.Int("cache_read_error_tokens", predicted-cached), zap.Float64("upstream_cache_hit_rate", float64(cached)/float64(input)), zap.Float64("simulated_cache_hit_rate", float64(predicted)/float64(input)))
			}
		} else {
			fields = append(fields, zap.Any("simulated_cache_read_tokens", nil))
		}
		if complete && !result.UsageUnavailable && input > 0 && cached >= 0 && cached <= input {
			committed = p.Commit(time.Now())
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
	activeTurn := 0
	priorObserve, priorAfter := wrapped.ObserveUpstreamRequest, wrapped.AfterTurn
	wrapped.ObserveUpstreamRequest = func(turn int, body []byte, routingHeaders ...http.Header) {
		if priorObserve != nil {
			priorObserve(turn, body, routingHeaders...)
		}
		mu.Lock()
		defer mu.Unlock()
		if active == nil || turn != activeTurn {
			// A new frame can replace a turn before its terminal callback arrives.
			// Finish the old owner before losing its in-flight fingerprints.
			active.finish(nil, context.Canceled)
			active = newPromptCacheShadowAttempt(&s.promptCacheShadow, e, c, account)
			activeTurn = turn
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
