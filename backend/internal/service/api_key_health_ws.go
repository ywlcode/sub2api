package service

import (
	"context"
	"errors"
	"net/http"
	"sync"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// Successful handshakes are not request samples. A failed foreground dial is
// one failed attempt; each subsequent response.create owns its own sample.
func acquireAPIKeyHealthWS(ctx context.Context, pool *openAIWSConnPool, limiter *RateLimitService, req openAIWSAcquireRequest) (*openAIWSConnLease, error) {
	observation, err := beginAPIKeyHealthAttempt(ctx, limiter, req.Account)
	if err != nil {
		return nil, err
	}
	lease, err := pool.Acquire(ctx, req)
	var dialErr *openAIWSDialError
	if errors.As(err, &dialErr) {
		observation.recordWSDialError(dialErr.StatusCode, dialErr.Err)
	}
	return lease, err
}

func (o *apiKeyHealthObservation) recordWSDialError(status int, err error) {
	if apiKeyHealthFailureStatus(status) {
		o.record(false, status, fmtAPIKeyHealthStatus(status))
	} else if status == 0 {
		o.recordTransportError(err)
	}
}

// Native WS readers and writers run concurrently. Keep health ownership at the
// actual upstream connection, outside HTTP bridges and handler-level retries.
type apiKeyHealthWSTracker struct {
	mu      sync.Mutex
	ctx     context.Context
	limiter *RateLimitService
	account *Account
	current *apiKeyHealthObservation
}

func newAPIKeyHealthWSTracker(ctx context.Context, limiter *RateLimitService, account *Account) *apiKeyHealthWSTracker {
	if limiter == nil || limiter.apiKeyHealth == nil || !account.APIKeyHealthConfig().Enabled {
		return nil
	}
	return &apiKeyHealthWSTracker{ctx: ctx, limiter: limiter, account: account}
}

func (h *apiKeyHealthWSTracker) beforeSend() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	observation, err := beginAPIKeyHealthAttempt(h.ctx, h.limiter, h.account)
	if err != nil {
		return err
	}
	h.current = observation
	return nil
}

func (h *apiKeyHealthWSTracker) ioError(err error) {
	if h == nil || err == nil || errors.Is(err, context.Canceled) && !apiKeyHealthUpstreamTimeout(h.ctx) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// A read/write error on an active turn proves an upstream interruption,
	// including WebSocket close frames and operation-specific timeouts.
	h.current.record(false, 0, "Upstream WebSocket connection failed or timed out")
	h.current = nil
}

func (h *apiKeyHealthWSTracker) receive(data []byte) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		return
	}
	// Some compatible upstreams concatenate complete JSON documents in a frame.
	if documents, repaired := splitOpenAIConcatenatedJSONDocuments(data); repaired {
		for _, document := range documents {
			h.receiveLocked(document)
		}
		return
	}
	h.receiveLocked(data)
}

func (h *apiKeyHealthWSTracker) receiveLocked(data []byte) {
	if h.current == nil {
		return
	}
	typ := gjson.GetBytes(data, "type").String()
	if !gjson.ValidBytes(data) || typ == "error" || typ == "response.failed" || apiKeyHealthErrorJSON(data) {
		h.current.record(false, http.StatusOK, "Upstream WebSocket returned an error response")
		h.current = nil
	} else if IsSuccessfulStreamTerminal(data) || typ == "response.done" && gjson.GetBytes(data, "response.status").String() == "completed" {
		h.current.record(true, http.StatusOK, "")
		h.current = nil
	} else if isOpenAIWSTerminalEvent(typ) {
		// A valid limited/cancelled terminal is not a broken connection. Exclude
		// ambiguous outcomes rather than blame the upstream for caller limits.
		h.current = nil
	}
}

type apiKeyHealthWSFrameConn struct {
	openaiwsv2.FrameConn
	health *apiKeyHealthWSTracker
}

func (c *apiKeyHealthWSFrameConn) WriteFrame(ctx context.Context, typ coderws.MessageType, data []byte) error {
	if gjson.GetBytes(data, "type").String() == "response.create" {
		if err := c.health.beforeSend(); err != nil {
			return err
		}
	}
	err := c.FrameConn.WriteFrame(ctx, typ, data)
	c.health.ioError(err)
	return err
}

func (c *apiKeyHealthWSFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	typ, data, err := c.FrameConn.ReadFrame(ctx)
	if err != nil {
		c.health.ioError(err)
	} else if typ == coderws.MessageText || typ == coderws.MessageBinary {
		c.health.receive(data)
	}
	return typ, data, err
}
