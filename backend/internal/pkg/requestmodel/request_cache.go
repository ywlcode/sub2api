package requestmodel

import (
	"context"
	"net/http"
	"slices"
	"sync"

	"github.com/tidwall/gjson"
)

type requestBodyCacheKey struct{}

// requestBodyCache is scoped to one HTTP request and the current immutable body.
// It caches parsing only, never a permission decision or an account-specific map.
// A replacement body invalidates all derived data, including JSON validity.
type requestBodyCache struct {
	mu          sync.Mutex
	body        []byte
	route       string
	contentType string
	models      []string
	modelsKnown bool
	valid       bool
	validKnown  bool
	released    bool
	fields      jsonBodyFields
	fieldsKnown bool
}

func bodyCache(req *http.Request) *requestBodyCache {
	if cached, ok := req.Context().Value(requestBodyCacheKey{}).(*requestBodyCache); ok {
		return cached
	}
	cached := &requestBodyCache{}
	*req = *req.WithContext(context.WithValue(req.Context(), requestBodyCacheKey{}, cached))
	return cached
}

func (c *requestBodyCache) useBody(body []byte) {
	same := len(c.body) == len(body) && (len(body) == 0 || &c.body[0] == &body[0])
	if !same {
		c.body = body
		c.models = nil
		c.modelsKnown = false
		c.validKnown = false
		c.fields = jsonBodyFields{}
		c.fieldsKnown = false
	}
}

// FromRequestCandidates reuses candidates across admission middleware without
// retaining additional body copies. Returned slices can safely be appended to
// or modified by callers. Body bytes must remain immutable; rewrites supply a
// new slice (ResetRequestBody is the normal way to publish one).
func FromRequestCandidates(req *http.Request, route string, body []byte) []string {
	if req == nil {
		return FromBodyCandidates(route, "", body)
	}
	cached := bodyCache(req)
	cached.mu.Lock()
	defer cached.mu.Unlock()
	if cached.released {
		return FromBodyCandidates(route, req.Header.Get("Content-Type"), body)
	}
	cached.useBody(body)
	contentType := req.Header.Get("Content-Type")
	if !isMultipartContentType(contentType) {
		fields := cached.jsonFields(body)
		models := fields.topLevel
		if IsLiveRequestRoute(route) {
			models = fields.session
			if len(models) == 0 {
				models = fields.topLevel
			}
		} else if len(models) == 0 {
			models = fields.session
		}
		return slices.Clone(models)
	}
	if !cached.modelsKnown || cached.route != route || cached.contentType != contentType {
		cached.models = FromBodyCandidates(route, contentType, body)
		cached.modelsKnown = true
		cached.route, cached.contentType = route, contentType
	}
	return slices.Clone(cached.models)
}

// ValidJSON shares the existing gjson validation result for unchanged bytes.
func ValidJSON(req *http.Request, body []byte) bool {
	if req == nil {
		return gjson.ValidBytes(body)
	}
	cached := bodyCache(req)
	cached.mu.Lock()
	defer cached.mu.Unlock()
	if cached.released {
		return gjson.ValidBytes(body)
	}
	cached.useBody(body)
	if !cached.validKnown {
		cached.valid = gjson.ValidBytes(body)
		cached.validKnown = true
	}
	return cached.valid
}

// Release drops body references when HTTP handling finishes. Detached billing or
// logging contexts may outlive the request; they must not pin large upload buffers.
func Release(req *http.Request) {
	if req == nil {
		return
	}
	cached, ok := req.Context().Value(requestBodyCacheKey{}).(*requestBodyCache)
	if !ok {
		return
	}
	cached.mu.Lock()
	defer cached.mu.Unlock()
	cached.body, cached.models = nil, nil
	cached.fields = jsonBodyFields{}
	cached.fieldsKnown = false
	cached.modelsKnown, cached.validKnown = false, false
	cached.released = true
}

// jsonFields requires the cache lock. Values own only small model strings.
func (c *requestBodyCache) jsonFields(body []byte) jsonBodyFields {
	if !c.fieldsKnown {
		c.fields = parseJSONBodyFields(body)
		c.fieldsKnown = true
	}
	return c.fields
}

// RequestsStream preserves the stream-only admission rule: every case-insensitive
// stream key must be true, with at least one occurrence in a valid JSON object.
func RequestsStream(req *http.Request, body []byte) bool {
	if req == nil {
		if !gjson.ValidBytes(body) {
			return false
		}
		fields := parseJSONBodyFields(body)
		return fields.object && fields.streamFound && fields.allStreaming
	}
	cached := bodyCache(req)
	cached.mu.Lock()
	defer cached.mu.Unlock()
	if cached.released {
		return RequestsStream(nil, body)
	}
	cached.useBody(body)
	if !cached.validKnown {
		cached.valid, cached.validKnown = gjson.ValidBytes(body), true
	}
	if !cached.valid {
		return false
	}
	fields := cached.jsonFields(body)
	return fields.object && fields.streamFound && fields.allStreaming
}
