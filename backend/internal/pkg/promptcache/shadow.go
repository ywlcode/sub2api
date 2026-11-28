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
		o.TTL = time.Hour
	}
	if o.MinPrefixTokens <= 0 {
		o.MinPrefixTokens = 1024
	}
	if o.PrefixTokensStep <= 0 {
		o.PrefixTokensStep = 128
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = 1000000
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 8 << 20
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
	anchor bool
}
type entry struct {
	key              [32]byte
	expires, written time.Time
	inputTokens      int
}

type Engine struct {
	prefixCache
	shared, warm        prefixCache
	anchors, opaqueSeen prefixCache
	mu                  sync.Mutex
	options             Options
	secret              [32]byte
	instanceID          string
	retainedPoints      int
	stateRevision       uint64
}

type prefixCache struct {
	entries                        map[[32]byte]*list.Element
	lru                            *list.List
	limit                          int
	evictions                      uint64
	liveEvictions, expiredReclaims uint64
}

func newPrefixCache(limit int) prefixCache {
	return prefixCache{entries: make(map[[32]byte]*list.Element), lru: list.New(), limit: limit}
}

const maxAttemptPoints = 16384
const maxRetainedPoints = 65536

func New(options Options) (*Engine, error) {
	opts := options.Defaults()
	accountLimit := max(1, opts.MaxEntries/4)
	anchorLimit := min(100000, opts.MaxEntries/10)
	e := &Engine{options: opts, prefixCache: newPrefixCache(accountLimit), shared: newPrefixCache(opts.MaxEntries - accountLimit - anchorLimit), warm: newPrefixCache(min(10000, max(16, opts.MaxEntries))), anchors: newPrefixCache(anchorLimit), opaqueSeen: newPrefixCache(min(10000, opts.MaxEntries))}
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
	engine                                                      *Engine
	points                                                      []point
	accountNamespace, sharedNamespace, warmNamespace            [32]byte
	EstimatedInputTokens                                        int
	CacheableInputTokens                                        int
	MatchedPrefixTokens                                         int
	SharedPrefixTokens                                          int
	MatchedInputTokens                                          int
	OpaqueBlocks                                                int
	OutputIndexed                                               bool
	recordHash, outputHash                                      [32]byte
	CacheAge                                                    time.Duration
	Decision, SkipReason, Protocol, Layout                      string
	PrefixFingerprint                                           string
	RoutingFingerprint                                          string
	AccountEvictions, SharedEvictions, WarmStateEvictions       uint64
	Expired                                                     bool
	AccountLiveEvictions, AccountExpiredReclaims                uint64
	AnchorStatus                                                string
	AnchorEvictions, AnchorLiveEvictions, AnchorExpiredReclaims uint64
	OpaqueHistoryEvictions                                      uint64
	MeasuredAnchorAge                                           time.Duration
	OpaqueKnownBlocks, OpaqueOutputKnownBlocks                  int
	opaqueKeys, outputOpaqueKeys                                [][32]byte
	ComputedIn                                                  time.Duration
	once                                                        sync.Once
	releaseOnce                                                 sync.Once
	reserved                                                    int
}

func (e *Engine) Begin(scope Scope, body []byte, encode Encode, now time.Time) *Attempt {
	start := time.Now()
	a := &Attempt{engine: e, Decision: "skipped"}
	defer func() { a.ComputedIn = time.Since(start) }()
	if len(body) > e.options.MaxBodyBytes {
		a.SkipReason = "body_limit"
		return a
	}
	parts, protocol, layout, identity, opaque, opaqueHashes, reason := e.promptParts(body, scope.Protocol)
	a.Protocol, a.Layout = protocol, layout
	a.OpaqueBlocks = opaque
	if reason != "" {
		a.SkipReason = reason
		return a
	}
	// Account-local routing identities are intentionally absent from the shared
	// candidate: it answers whether upstream account switching explains a miss.
	base, _ := json.Marshal([]any{scope.Tenant, scope.Platform, scope.Model, protocol, layout})
	a.sharedNamespace = e.digest(base)
	for _, hash := range opaqueHashes {
		a.opaqueKeys = append(a.opaqueKeys, e.opaqueKey(a.sharedNamespace, hash))
	}
	account, _ := json.Marshal([]any{hex.EncodeToString(a.sharedNamespace[:]), scope.AccountID, identity, scope.RoutingFingerprint})
	a.accountNamespace = e.digest(account)
	a.RoutingFingerprint = scope.RoutingFingerprint
	warm, _ := json.Marshal([]any{scope.Tenant, scope.Platform, scope.Model})
	a.warmNamespace = e.digest(warm)
	h := hmac.New(sha256.New, e.secret[:])
	var token [8]byte
	for _, part := range parts {
		ids, err := encode(part)
		if err != nil {
			a.SkipReason = "tokenizer_error"
			a.points = nil
			return a
		}
		for _, id := range ids {
			binary.LittleEndian.PutUint64(token[:], uint64(id))
			_, _ = h.Write(token[:])
			a.EstimatedInputTokens++
			n := a.EstimatedInputTokens
			if n >= e.options.MinPrefixTokens && (n-e.options.MinPrefixTokens)%e.options.PrefixTokensStep == 0 {
				if len(a.points) >= maxAttemptPoints {
					a.points = nil
					a.SkipReason = "fingerprint_limit"
					return a
				}
				var hash [32]byte
				copy(hash[:], h.Sum(nil))
				a.points = append(a.points, point{hash: hash, tokens: n})
			}
		}
		a.recordHash = e.recordDigest(a.recordHash, []byte(part))
		if a.EstimatedInputTokens > 0 {
			if len(a.points) >= maxAttemptPoints {
				a.points = nil
				a.SkipReason = "fingerprint_limit"
				return a
			}
			a.points = append(a.points, point{hash: a.recordHash, tokens: a.EstimatedInputTokens, anchor: true})
		}
	}
	a.CacheableInputTokens = a.EstimatedInputTokens
	if a.EstimatedInputTokens == 0 {
		a.SkipReason = "empty_input"
		return a
	}
	var full [32]byte
	copy(full[:], h.Sum(nil))
	a.PrefixFingerprint = hex.EncodeToString(full[:8])
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(a.points)+len(a.opaqueKeys) > maxAttemptPoints {
		a.points, a.opaqueKeys = nil, nil
		a.SkipReason = "fingerprint_limit"
		return a
	}
	if e.retainedPoints+len(a.points)+len(a.opaqueKeys) > maxRetainedPoints {
		a.points = nil
		a.opaqueKeys = nil
		a.SkipReason = "observer_budget"
		return a
	}
	a.reserved = len(a.points) + len(a.opaqueKeys)
	e.retainedPoints += a.reserved
	// Freeze immediately before submission, after tokenization. A concurrent
	// successful request may have refreshed a prefix while we were encoding.
	now = now.Add(time.Since(start))
	_, seen := e.warm.lookup(a.warmNamespace, now)
	a.AccountEvictions, a.SharedEvictions, a.WarmStateEvictions = e.evictions, e.shared.evictions, e.warm.evictions
	a.AccountLiveEvictions, a.AccountExpiredReclaims = e.liveEvictions, e.expiredReclaims
	a.AnchorEvictions, a.AnchorLiveEvictions, a.AnchorExpiredReclaims = e.anchors.evictions, e.anchors.liveEvictions, e.anchors.expiredReclaims
	a.OpaqueHistoryEvictions = e.opaqueSeen.evictions
	a.AnchorStatus = "not_found"
	for _, key := range a.opaqueKeys {
		if item, ok := e.opaqueSeen.lookup(key, now); ok {
			a.OpaqueKnownBlocks++
			if item.inputTokens == 2 {
				a.OpaqueOutputKnownBlocks++
			}
		}
	}
	a.Decision = "cold"
	if seen {
		a.Decision = "miss"
	}
	for i := len(a.points) - 1; i >= 0; i-- {
		key := e.key(a.accountNamespace, a.points[i].hash)
		if item, ok := e.lookup(key, now); ok {
			if a.points[i].anchor && a.points[i].tokens < e.options.MinPrefixTokens && item.inputTokens < e.options.MinPrefixTokens {
				continue
			}
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
		if item, ok := e.shared.lookup(e.key(a.sharedNamespace, a.points[i].hash), now); ok {
			if a.points[i].anchor && a.points[i].tokens < e.options.MinPrefixTokens && item.inputTokens < e.options.MinPrefixTokens {
				continue
			}
			a.SharedPrefixTokens = a.points[i].tokens
			break
		}
	}
	// Record boundaries carry historical measured input totals. These anchors
	// remain useful when opaque blocks have no locally countable plaintext.
	for _, p := range a.points {
		if !p.anchor {
			continue
		}
		key := e.key(a.sharedNamespace, p.hash)
		if item, ok := e.anchors.lookup(key, now); ok && item.inputTokens >= e.options.MinPrefixTokens {
			if item.inputTokens > a.MatchedInputTokens {
				a.MatchedInputTokens, a.MeasuredAnchorAge = item.inputTokens, now.Sub(item.written)
			}
		} else if elem := e.anchors.entries[key]; elem != nil {
			item, _ := elem.Value.(entry)
			if !now.Before(item.expires) {
				a.AnchorStatus = "expired"
			}
		}
		for _, candidate := range []struct {
			cache *prefixCache
			ns    [32]byte
		}{{&e.prefixCache, a.accountNamespace}, {&e.shared, a.sharedNamespace}} {
			if item, ok := candidate.cache.lookup(e.key(candidate.ns, p.hash), now); ok && item.inputTokens >= e.options.MinPrefixTokens {
				if item.inputTokens > a.MatchedInputTokens {
					a.MatchedInputTokens, a.MeasuredAnchorAge = item.inputTokens, now.Sub(item.written)
				}
			}
		}
	}
	if a.MatchedInputTokens > 0 {
		a.AnchorStatus = "hit"
		a.Decision = "hit"
	}
	if a.SharedPrefixTokens > 0 {
		a.Decision = "hit"
	}
	if a.OpaqueBlocks > 0 && max(a.MatchedPrefixTokens, a.SharedPrefixTokens) < a.EstimatedInputTokens && a.MatchedInputTokens == 0 {
		// A matching plaintext fragment cannot quantify an opaque continuation.
		a.Decision = "cold"
		if seen {
			a.Decision = "miss"
		}
	}
	return a
}

// Commit only follows a successful, metered completion. The observer never
// writes user balances, billing tokens, responses, or upstream cache counters.
func (a *Attempt) Commit(now time.Time) (committed bool) {
	return a.CommitUsage(0, 0, now)
}

// CommitUsage uses historical total input/output usage only; the upstream
// cached-token counter is deliberately not accepted by this API.
func (a *Attempt) CommitUsage(inputTokens, outputTokens int, now time.Time) (committed bool) {
	if a == nil || a.SkipReason != "" {
		return false
	}
	defer a.Release()
	a.once.Do(func() {
		e := a.engine
		e.mu.Lock()
		defer e.mu.Unlock()
		e.stateRevision++
		// Warmth describes the user's logical model cache, independent of an
		// upstream account, routing key or evictable prefix.
		e.warm.put(a.warmNamespace, now, 0)
		for _, p := range a.points {
			measured := 0
			if p.anchor && p.hash == a.recordHash {
				measured = max(0, inputTokens)
			}
			if p.anchor {
				key := e.key(a.sharedNamespace, p.hash)
				anchorMeasured := measured
				if item, ok := e.anchors.lookup(key, now); ok {
					anchorMeasured = max(anchorMeasured, item.inputTokens)
				}
				// Successful continued use renews the measured prefix, matching
				// the ordinary index TTL. Failed requests cannot renew it.
				if anchorMeasured >= e.options.MinPrefixTokens {
					e.anchors.putMeasured(key, now, e.options.TTL, anchorMeasured)
				}
			}
			e.putMeasured(e.key(a.accountNamespace, p.hash), now, e.options.TTL, measured)
			e.shared.putMeasured(e.key(a.sharedNamespace, p.hash), now, e.options.TTL, measured)
		}
		if a.OutputIndexed && inputTokens > 0 && outputTokens >= 0 && outputTokens <= int(^uint(0)>>1)-inputTokens {
			for _, candidate := range []struct {
				cache *prefixCache
				ns    [32]byte
			}{{&e.prefixCache, a.accountNamespace}, {&e.shared, a.sharedNamespace}} {
				candidate.cache.putMeasured(e.key(candidate.ns, a.outputHash), now, e.options.TTL, inputTokens+outputTokens)
			}
			if inputTokens+outputTokens >= e.options.MinPrefixTokens {
				e.anchors.putMeasured(e.key(a.sharedNamespace, a.outputHash), now, e.options.TTL, inputTokens+outputTokens)
			}
		}
		for _, key := range a.opaqueKeys {
			e.opaqueSeen.putMeasured(key, now, e.options.TTL, 1)
		}
		for _, key := range a.outputOpaqueKeys {
			e.opaqueSeen.putMeasured(key, now, e.options.TTL, 2)
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
		a.opaqueKeys, a.outputOpaqueKeys = nil, nil
	})
}

// Opaque identities are diagnostic only, scoped to the same logical user and
// model as the measured anchors. They never infer the length of ciphertext.
func (e *Engine) opaqueKey(namespace, hash [32]byte) [32]byte {
	var raw [65]byte
	raw[0] = 4
	copy(raw[1:33], namespace[:])
	copy(raw[33:], hash[:])
	return e.digest(raw[:])
}

func (e *Engine) AnchorCapacities() (anchors, opaque int) { return e.anchors.limit, e.opaqueSeen.limit }

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
	e.putMeasured(key, now, ttl, 0)
}
func (e *prefixCache) putMeasured(key [32]byte, now time.Time, ttl time.Duration, measured int) {
	if e.limit <= 0 {
		return
	}
	item := entry{key: key, written: now, inputTokens: measured}
	if ttl > 0 {
		item.expires = now.Add(ttl)
	}
	if elem := e.entries[key]; elem != nil {
		if prior, ok := elem.Value.(entry); ok {
			item.inputTokens = max(item.inputTokens, prior.inputTokens)
		}
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
		if !evicted.expires.IsZero() && !now.Before(evicted.expires) {
			e.expiredReclaims++
		} else {
			e.liveEvictions++
		}
	}
	e.entries[key] = e.lru.PushFront(item)
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

// Compare expresses the frozen prefix match in upstream input-token units.
// The prefix fraction is independent of upstream cached-token counts. Keep the
// unscaled tokenizer count separately so serialization/tokenizer drift remains
// visible instead of confusing estimated tokens with upstream billing tokens.
func (a *Attempt) Compare(inputTokens int) (predicted, raw int) {
	if a == nil || a.SkipReason != "" || inputTokens <= 0 || a.EstimatedInputTokens <= 0 || a.MatchedPrefixTokens < 0 {
		return 0, 0
	}
	raw = max(a.MatchedPrefixTokens, a.SharedPrefixTokens)
	if a.engine != nil && inputTokens < a.engine.options.MinPrefixTokens {
		return 0, raw
	}
	if raw >= a.EstimatedInputTokens {
		return inputTokens, raw
	}
	// An opaque HMAC is an identity marker, not a token-length estimate. Scaling
	// a short matched plaintext prefix by a total that includes unseen encrypted
	// context would silently count that context as cached. Only measured record
	// anchors can quantify a partial opaque prompt; an exact full-input match
	// above still represents complete local reuse.
	if a.OpaqueBlocks > 0 {
		return min(inputTokens, max(0, a.MatchedInputTokens)), raw
	}
	// Exact floor(input * matched / estimated), without float rounding or int
	// overflow. Since matched < estimated, the quotient is bounded by input and
	// Div64's high-word precondition is satisfied for every positive int input.
	hi, lo := bits.Mul64(uint64(inputTokens), uint64(raw))
	quotient, _ := bits.Div64(hi, lo, uint64(a.EstimatedInputTokens))
	predicted = min(inputTokens, max(int(quotient), a.MatchedInputTokens))
	return
}
