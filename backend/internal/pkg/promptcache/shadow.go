// Package promptcache estimates prompt-prefix reuse for observation only.
// It stores keyed fingerprints and counters, never prompts or model responses.
package promptcache

import (
	"bytes"
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/bits"
	"strings"
	"sync"
	"time"
)

type Options struct {
	TTL                                                         time.Duration
	MinPrefixTokens, PrefixTokensStep, MaxEntries, MaxBodyBytes int
}

func (o Options) Defaults() Options {
	if o.TTL <= 0 {
		o.TTL = 10 * time.Minute
	}
	if o.MinPrefixTokens <= 0 {
		o.MinPrefixTokens = 1024
	}
	if o.PrefixTokensStep <= 0 {
		o.PrefixTokensStep = 128
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = 100000
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 2 << 20
	}
	return o
}

type Scope struct {
	Tenant, Platform, Model string
	AccountID               int64
	Protocol                string
	RoutingFingerprint      string
}

const (
	ProtocolResponses       = "responses"
	ProtocolChatCompletions = "chat_completions"
	ProtocolMessages        = "messages"
)

type Encode func(string) ([]uint, error)

type point struct {
	hash   [32]byte
	tokens int
}
type entry struct {
	key              [32]byte
	expires, written time.Time
}

type Engine struct {
	prefixCache
	shared, warm   prefixCache
	mu             sync.Mutex
	options        Options
	secret         [32]byte
	instanceID     string
	retainedPoints int
}

type prefixCache struct {
	entries   map[[32]byte]*list.Element
	lru       *list.List
	limit     int
	evictions uint64
}

func newPrefixCache(limit int) prefixCache {
	return prefixCache{entries: make(map[[32]byte]*list.Element), lru: list.New(), limit: limit}
}

const maxAttemptPoints = 16384
const maxRetainedPoints = 65536

func New(options Options) (*Engine, error) {
	opts := options.Defaults()
	accountLimit := max(1, opts.MaxEntries*3/4)
	e := &Engine{options: opts, prefixCache: newPrefixCache(accountLimit), shared: newPrefixCache(opts.MaxEntries - accountLimit), warm: newPrefixCache(min(10000, max(16, opts.MaxEntries)))}
	if _, err := rand.Read(e.secret[:]); err != nil {
		return nil, err
	}
	instance := sha256.Sum256(e.secret[:])
	e.instanceID = hex.EncodeToString(instance[:8])
	return e, nil
}

func (e *Engine) InstanceID() string { return e.instanceID }
func (e *Engine) Options() Options   { return e.options }
func (e *Engine) MaxBodyBytes() int  { return e.options.MaxBodyBytes }
func (e *Engine) CacheCapacities() (account, shared, warm int) {
	return e.limit, e.shared.limit, e.warm.limit
}

// RoutingFingerprint hashes actual outbound routing headers. Raw session or
// conversation identifiers are not retained by the cache or written to logs.
func (e *Engine) RoutingFingerprint(session, conversation string) string {
	if session == "" && conversation == "" {
		return ""
	}
	raw, _ := json.Marshal([]string{session, conversation})
	digest := e.digest(raw)
	return hex.EncodeToString(digest[:])
}

// Attempt contains fingerprints only and freezes the lookup before submission.
// Concurrent requests cannot claim their own future cache writes as hits.
type Attempt struct {
	engine                                                *Engine
	points                                                []point
	accountNamespace, sharedNamespace, warmNamespace      [32]byte
	EstimatedInputTokens                                  int
	CacheableInputTokens                                  int
	MatchedPrefixTokens                                   int
	SharedPrefixTokens                                    int
	CacheAge                                              time.Duration
	Decision, SkipReason, Protocol, Layout                string
	PrefixFingerprint                                     string
	RoutingFingerprint                                    string
	AccountEvictions, SharedEvictions, WarmStateEvictions uint64
	Expired                                               bool
	ComputedIn                                            time.Duration
	once                                                  sync.Once
	releaseOnce                                           sync.Once
	reserved                                              int
}

func (e *Engine) Begin(scope Scope, body []byte, encode Encode, now time.Time) *Attempt {
	start := time.Now()
	a := &Attempt{engine: e, Decision: "skipped"}
	defer func() { a.ComputedIn = time.Since(start) }()
	if len(body) > e.options.MaxBodyBytes {
		a.SkipReason = "body_limit"
		return a
	}
	prompt, protocol, layout, identity, reason := promptText(body, scope.Protocol)
	a.Protocol, a.Layout = protocol, layout
	if reason != "" {
		a.SkipReason = reason
		return a
	}
	ids, err := encode(prompt)
	if err != nil {
		a.SkipReason = "tokenizer_error"
		return a
	}
	a.EstimatedInputTokens = len(ids)
	a.CacheableInputTokens = len(ids)
	if len(ids) == 0 {
		a.SkipReason = "empty_input"
		return a
	}
	// Account-local routing identities are intentionally absent from the shared
	// candidate: it answers whether upstream account switching explains a miss.
	base, _ := json.Marshal([]any{scope.Tenant, scope.Platform, scope.Model, protocol, layout})
	a.sharedNamespace = e.digest(base)
	account, _ := json.Marshal([]any{hex.EncodeToString(a.sharedNamespace[:]), scope.AccountID, identity, scope.RoutingFingerprint})
	a.accountNamespace = e.digest(account)
	a.RoutingFingerprint = scope.RoutingFingerprint
	warm, _ := json.Marshal([]any{scope.Platform, scope.AccountID, scope.Model})
	a.warmNamespace = e.digest(warm)
	h := hmac.New(sha256.New, e.secret[:])
	var token [8]byte
	for i, id := range ids {
		binary.LittleEndian.PutUint64(token[:], uint64(id))
		_, _ = h.Write(token[:])
		n := i + 1
		if n >= e.options.MinPrefixTokens && (n-e.options.MinPrefixTokens)%e.options.PrefixTokensStep == 0 {
			if len(a.points) >= maxAttemptPoints {
				a.points = nil
				a.SkipReason = "fingerprint_limit"
				return a
			}
			var hash [32]byte
			copy(hash[:], h.Sum(nil))
			a.points = append(a.points, point{hash, n})
		}
	}
	var full [32]byte
	copy(full[:], h.Sum(nil))
	a.PrefixFingerprint = hex.EncodeToString(full[:8])
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retainedPoints+len(a.points) > maxRetainedPoints {
		a.points = nil
		a.SkipReason = "observer_budget"
		return a
	}
	a.reserved = len(a.points)
	e.retainedPoints += a.reserved
	// Freeze immediately before submission, after tokenization. A concurrent
	// successful request may have refreshed a prefix while we were encoding.
	now = now.Add(time.Since(start))
	_, seen := e.warm.lookup(a.warmNamespace, now)
	a.AccountEvictions, a.SharedEvictions, a.WarmStateEvictions = e.evictions, e.shared.evictions, e.warm.evictions
	a.Decision = "cold"
	if seen {
		a.Decision = "miss"
	}
	for i := len(a.points) - 1; i >= 0; i-- {
		key := e.key(a.accountNamespace, a.points[i].hash)
		if item, ok := e.lookup(key, now); ok {
			a.MatchedPrefixTokens = a.points[i].tokens
			a.CacheAge = now.Sub(item.written)
			a.Decision = "hit"
			break
		}
		if elem := e.entries[key]; elem != nil {
			item, ok := elem.Value.(entry)
			if ok && !now.Before(item.expires) {
				a.Expired = true
			}
		}
	}
	for i := len(a.points) - 1; i >= 0; i-- {
		if _, ok := e.shared.lookup(e.key(a.sharedNamespace, a.points[i].hash), now); ok {
			a.SharedPrefixTokens = a.points[i].tokens
			break
		}
	}
	return a
}

// Commit only follows a successful, metered completion. The observer never
// writes user balances, billing tokens, responses, or upstream cache counters.
func (a *Attempt) Commit(now time.Time) (committed bool) {
	if a == nil || a.SkipReason != "" {
		return false
	}
	defer a.Release()
	a.once.Do(func() {
		e := a.engine
		e.mu.Lock()
		defer e.mu.Unlock()
		// Warmth describes the observed account/model, not a particular user,
		// session key or evictable prefix. Expiry/capacity misses stay in warm_only.
		e.warm.put(a.warmNamespace, now, 0)
		for _, p := range a.points {
			e.put(e.key(a.accountNamespace, p.hash), now)
			e.shared.put(e.key(a.sharedNamespace, p.hash), now, e.options.TTL)
		}
		committed = true
	})
	return committed
}

// Release frees the bounded in-flight fingerprint reservation on every terminal
// path, including failures/retries; immutable diagnostic counters stay readable.
func (a *Attempt) Release() {
	if a == nil || a.engine == nil {
		return
	}
	a.releaseOnce.Do(func() {
		e := a.engine
		e.mu.Lock()
		defer e.mu.Unlock()
		e.retainedPoints -= a.reserved
		a.reserved = 0
		a.points = nil
	})
}

func (e *Engine) digest(data []byte) (out [32]byte) {
	h := hmac.New(sha256.New, e.secret[:])
	_, _ = h.Write(data)
	copy(out[:], h.Sum(nil))
	return
}
func (e *Engine) key(namespace, hash [32]byte) [32]byte {
	var raw [64]byte
	copy(raw[:32], namespace[:])
	copy(raw[32:], hash[:])
	return e.digest(raw[:])
}
func (e *prefixCache) lookup(key [32]byte, now time.Time) (entry, bool) {
	if elem := e.entries[key]; elem != nil {
		item, ok := elem.Value.(entry)
		if ok && !item.written.After(now) && (item.expires.IsZero() || now.Before(item.expires)) {
			e.lru.MoveToFront(elem)
			return item, true
		}
	}
	return entry{}, false
}
func (e *Engine) put(key [32]byte, now time.Time) {
	e.prefixCache.put(key, now, e.options.TTL)
}
func (e *prefixCache) put(key [32]byte, now time.Time, ttl time.Duration) {
	if e.limit <= 0 {
		return
	}
	item := entry{key: key, written: now}
	if ttl > 0 {
		item.expires = now.Add(ttl)
	}
	if elem := e.entries[key]; elem != nil {
		elem.Value = item
		e.lru.MoveToFront(elem)
		return
	}
	for len(e.entries) >= e.limit {
		last := e.lru.Back()
		// This private list only contains entry values inserted below.
		evicted, _ := last.Value.(entry)
		delete(e.entries, evicted.key)
		e.lru.Remove(last)
		e.evictions++
	}
	e.entries[key] = e.lru.PushFront(item)
}

// promptText creates an explicitly approximate wire-prompt representation.
// JSON object order/formatting is canonicalized, but content strings and array
// order are preserved. Non-prompt controls (stream, request ids) do not pollute
// the prefix. No timestamps, paths, tool results or system text are scrubbed.
func promptText(body []byte, hint string) (text, protocol, layout, identity, reason string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		reason = "invalid_json"
		return
	}
	for _, field := range []string{"previous_response_id", "conversation", "conversation_id", "thread_id", "cached_content", "cachedContent"} {
		if nonempty(root[field]) {
			reason = "server_side_context"
			return
		}
	}
	for _, field := range []string{"prompt"} {
		if nonempty(root[field]) {
			reason = "stored_prompt"
			return
		}
	}
	for _, field := range []string{"input", "messages", "system", "tools", "instructions"} {
		if unsupported(root[field]) {
			reason = "opaque_or_media_input"
			return
		}
	}
	var sequence []json.RawMessage
	appendField := func(name string) {
		if nonempty(root[name]) {
			tag, _ := json.Marshal(name)
			sequence = append(sequence, tag, root[name])
		}
	}
	appendField("tools")
	if _, ok := root["messages"]; ok && hint != ProtocolResponses {
		protocol = ProtocolChatCompletions
		layout = "tools_system_messages"
		if hint == ProtocolMessages || hint == "" && nonempty(root["system"]) {
			protocol = ProtocolMessages
			appendField("system")
		}
		appendField("messages")
	} else if _, ok := root["input"]; ok {
		protocol = ProtocolResponses
		layout = "instructions_before_input"
		// This is an explicit candidate layout, not a claim about a private
		// provider's chat template. Compare before adding model-specific rules.
		appendField("instructions")
		appendField("input")
	} else {
		reason = "unsupported_protocol"
		return
	}
	if hint != "" && protocol != hint {
		reason = "protocol_mismatch"
		return
	}
	// Output schema and tool selection can affect provider prompt construction.
	// Conservatively scope them instead of letting a change produce a false hit.
	controls := map[string]json.RawMessage{}
	for _, key := range []string{"tool_choice", "parallel_tool_calls", "response_format", "text", "reasoning", "thinking"} {
		if nonempty(root[key]) {
			controls[key] = root[key]
		}
	}
	controlsRaw, _ := canonicalJSON(controls)
	var decodedControls any
	_ = decodeExactJSON(controlsRaw, &decodedControls)
	canonicalControls, _ := canonicalJSON(decodedControls)
	var cacheIdentity any
	_ = decodeExactJSON(root["prompt_cache_key"], &cacheIdentity)
	identityJSON, _ := json.Marshal(cacheIdentity)
	identity = string(identityJSON)
	var out strings.Builder
	_, _ = out.Write(canonicalControls)
	_ = out.WriteByte('\n')
	for _, raw := range sequence {
		var value any
		if decodeExactJSON(raw, &value) != nil {
			reason = "invalid_prompt"
			return
		}
		canonical, err := canonicalJSON(value)
		if err != nil {
			reason = "invalid_prompt"
			return
		}
		_, _ = out.Write(canonical)
		_ = out.WriteByte('\n')
	}
	text = out.String()
	return
}

func canonicalJSON(value any) ([]byte, error) { return json.Marshal(value) }
func decodeExactJSON(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(value)
}
func nonempty(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null" && v != "\"\"" && v != "[]" && v != "{}"
}
func unsupported(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["encrypted_content"]; ok {
				return true
			}
			if _, ok := x["signature"]; ok {
				return true
			}
			if kind, _ := x["type"].(string); kind != "" {
				switch kind {
				case "compaction", "item_reference", "redacted_thinking", "input_image", "output_image", "image_url", "image", "input_audio", "output_audio", "audio", "input_file", "file", "video", "video_url", "input_video":
					return true
				}
			}
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

// Compare expresses the frozen prefix match in upstream input-token units.
// The prefix fraction is independent of upstream cached-token counts. Keep the
// unscaled tokenizer count separately so serialization/tokenizer drift remains
// visible instead of confusing estimated tokens with upstream billing tokens.
func (a *Attempt) Compare(inputTokens int) (predicted, raw int) {
	if a == nil || a.SkipReason != "" || inputTokens <= 0 || a.EstimatedInputTokens <= 0 || a.MatchedPrefixTokens < 0 {
		return 0, 0
	}
	raw = a.MatchedPrefixTokens
	if raw >= a.EstimatedInputTokens {
		return inputTokens, raw
	}
	// Exact floor(input * matched / estimated), without float rounding or int
	// overflow. Since matched < estimated, the quotient is bounded by input and
	// Div64's high-word precondition is satisfied for every positive int input.
	hi, lo := bits.Mul64(uint64(inputTokens), uint64(raw))
	quotient, _ := bits.Div64(hi, lo, uint64(a.EstimatedInputTokens))
	predicted = int(quotient)
	return
}
