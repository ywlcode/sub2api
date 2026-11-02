package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Bind observation to physical sends, including service-internal retries. The
// decorator is deliberately absent from quota, discovery and admin-test traffic.
func apiKeyHealthHTTP(upstream HTTPUpstream, limiter *RateLimitService, account *Account) HTTPUpstream {
	if limiter == nil || limiter.apiKeyHealth == nil || !account.APIKeyHealthConfig().Enabled {
		return upstream
	}
	return &apiKeyHealthUpstream{HTTPUpstream: upstream, limiter: limiter, account: account}
}

type apiKeyHealthUpstream struct {
	HTTPUpstream
	limiter *RateLimitService
	account *Account
}

func (u *apiKeyHealthUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	return u.send(req, func() (*http.Response, error) { return u.HTTPUpstream.Do(req, proxyURL, accountID, concurrency) })
}

func (u *apiKeyHealthUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req, func() (*http.Response, error) {
		return u.HTTPUpstream.DoWithTLS(req, proxyURL, accountID, concurrency, profile)
	})
}

func (u *apiKeyHealthUpstream) send(req *http.Request, send func() (*http.Response, error)) (*http.Response, error) {
	ctx := req.Context()
	observation, err := beginAPIKeyHealthAttempt(ctx, u.limiter, u.account)
	if err != nil {
		return nil, err
	}
	resp, err := send()
	if err != nil {
		observation.recordTransportError(err)
		return resp, err
	}
	if resp != nil && observation != nil {
		request := req
		if resp.Request != nil {
			request = resp.Request
		}
		resp.Request = request.WithContext(context.WithValue(request.Context(), apiKeyHealthObservationKey{}, observation))
		// Status failures are definitive as soon as headers arrive. Count before
		// any retry can start, even if a caller defers closing the error body.
		if apiKeyHealthFailureStatus(resp.StatusCode) {
			observation.record(false, resp.StatusCode, fmtAPIKeyHealthStatus(resp.StatusCode))
			return resp, nil
		}
		body := &apiKeyHealthBody{ReadCloser: resp.Body, observation: observation, status: resp.StatusCode,
			stream: strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"),
			json:   strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json")}
		if resp.Body == nil {
			body.eof = true
			body.finish()
		} else {
			resp.Body = body
		}
	}
	return resp, nil
}

func beginAPIKeyHealthAttempt(ctx context.Context, limiter *RateLimitService, account *Account) (*apiKeyHealthObservation, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if limiter == nil || limiter.apiKeyHealth == nil || !account.APIKeyHealthConfig().Enabled {
		return nil, nil
	}
	state, err := limiter.apiKeyHealth.BeginAPIKeyHealthAttempt(ctx, account.ID, account.APIKeyHealthConfig())
	if err != nil {
		logger.L().Warn("apikey.health_breaker_begin_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, nil
	}
	if state == nil {
		return nil, nil
	}
	if state.UntilUnix > 0 {
		if account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Unix() < state.UntilUnix {
			limiter.persistAPIKeyHealthTrip(ctx, account, state)
		}
		return nil, &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable,
			Reason: APIKeyHealthBreakerReason, NextAccountAction: NextAccountRetry,
			ResponseBody: []byte(`{"error":{"type":"upstream_error","message":"Account health cooldown is active"}}`)}
	}
	// Account snapshots can be shared by schedulers; retain an immutable copy.
	snapshot := *account
	return &apiKeyHealthObservation{limiter: limiter, account: &snapshot, ctx: ctx, generation: state.Generation}, nil
}

type apiKeyHealthObservation struct {
	limiter    *RateLimitService
	account    *Account
	ctx        context.Context
	generation int64
	once       sync.Once
}

type apiKeyHealthObservationKey struct{}
type apiKeyHealthCallerContextKey struct{}

// Scanner timers can fire without a transport read error. Settle the physical
// response's observation before local cleanup cancels its upstream context.
func recordAPIKeyHealthResponseFailure(ctx context.Context, resp *http.Response, message string) {
	if resp == nil || resp.Request == nil || ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	observation, _ := resp.Request.Context().Value(apiKeyHealthObservationKey{}).(*apiKeyHealthObservation)
	observation.record(false, resp.StatusCode, message)
}

func (o *apiKeyHealthObservation) record(success bool, status int, message string) {
	if o == nil {
		return
	}
	o.once.Do(func() {
		if errors.Is(o.ctx.Err(), context.Canceled) && !apiKeyHealthUpstreamTimeout(o.ctx) {
			return
		}
		if caller, ok := o.ctx.Value(apiKeyHealthCallerContextKey{}).(context.Context); ok && errors.Is(caller.Err(), context.Canceled) {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), 3*time.Second)
		defer cancel()
		state, err := o.limiter.apiKeyHealth.RecordAPIKeyHealthResult(ctx, o.account.ID, o.generation, success, status, message)
		if err != nil {
			logger.L().Warn("apikey.health_breaker_record_failed", zap.Int64("account_id", o.account.ID), zap.Error(err))
			return
		}
		if state != nil && state.Tripped {
			o.limiter.persistAPIKeyHealthTrip(ctx, o.account, state)
		}
	})
}

func (o *apiKeyHealthObservation) recordTransportError(err error) {
	if o == nil || err == nil {
		return
	}
	var netErr net.Error
	upstreamTimeout := apiKeyHealthUpstreamTimeout(o.ctx)
	if upstreamTimeout || !errors.Is(err, context.Canceled) && (errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || classifyUpstreamTransportError(err).Persistent) {
		o.record(false, 0, "Upstream connection failed or timed out")
	}
}

func apiKeyHealthFailureStatus(status int) bool {
	return status == 400 || status == 408 || status == 429 || status >= 500 && status <= 599
}

func apiKeyHealthUpstreamTimeout(ctx context.Context) bool {
	var phaseErr *openAIHTTPPhaseTimeout
	var timeoutErr net.Error
	cause := context.Cause(ctx)
	return errors.As(cause, &phaseErr) || errors.Is(cause, context.DeadlineExceeded) || errors.As(cause, &timeoutErr) && timeoutErr.Timeout()
}

// Inspection is bounded and never changes response bytes. Long successful
// payloads still count; only structural error fields and SSE framing are read.
type apiKeyHealthBody struct {
	io.ReadCloser
	mu            sync.Mutex
	observation   *apiKeyHealthObservation
	status        int
	stream        bool
	json          bool
	buf           []byte
	oversized     bool
	unknownStream bool
	failed        bool
	terminal      bool
	eof           bool
	readErr       error
	closeOnce     sync.Once
	closeErr      error
}

func (b *apiKeyHealthBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stream && b.status < 400 {
		for _, part := range bytes.SplitAfter(p[:n], []byte{'\n'}) {
			remaining := priorityAttemptInspectBytes - len(b.buf)
			b.buf = append(b.buf, part[:min(len(part), remaining)]...)
			b.oversized = b.oversized || len(part) > remaining
			if bytes.HasSuffix(part, []byte{'\n'}) {
				if b.oversized {
					b.observeOversizedLine(b.buf)
				} else {
					b.observeLine(b.buf)
				}
				b.buf = b.buf[:0]
				b.oversized = false
			}
		}
	} else {
		remaining := priorityAttemptInspectBytes - len(b.buf)
		b.buf = append(b.buf, p[:min(n, remaining)]...)
		b.oversized = b.oversized || n > remaining
	}
	if err == io.EOF {
		b.eof = true
		if b.stream && len(b.buf) > 0 {
			if b.oversized {
				b.observeOversizedLine(b.buf)
			} else {
				b.observeLine(b.buf)
			}
		}
	} else if err != nil {
		b.readErr = err
	}
	if err != nil || b.stream && (b.failed || b.terminal) {
		b.finish()
	}
	return n, err
}

func apiKeyHealthErrorJSON(data []byte) bool {
	for _, path := range []string{"error", "response.error"} {
		value := gjson.GetBytes(data, path)
		if value.Exists() && value.Type != gjson.Null && value.Raw != "false" && value.Raw != `""` {
			return true
		}
	}
	return gjson.GetBytes(data, "status").String() == "failed" || gjson.GetBytes(data, "response.status").String() == "failed"
}

func (b *apiKeyHealthBody) observeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.Equal(line, []byte("event: error")) {
		b.failed = true
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(data, []byte("[DONE]")) {
		b.terminal = true
		return
	}
	if len(data) == 0 {
		return
	}
	if !gjson.ValidBytes(data) {
		b.failed = true
		return
	}
	typ := gjson.GetBytes(data, "type").String()
	b.failed = b.failed || typ == "error" || typ == "response.failed" || apiKeyHealthErrorJSON(data)
	b.terminal = b.terminal || typ == "message_stop" || IsSuccessfulStreamTerminal(data) ||
		typ == "response.done" && gjson.GetBytes(data, "response.status").String() == "completed"
	for _, choice := range gjson.GetBytes(data, "choices").Array() {
		b.terminal = b.terminal || choice.Get("finish_reason").String() != ""
	}
	for _, candidate := range gjson.GetBytes(data, "candidates").Array() {
		b.terminal = b.terminal || candidate.Get("finishReason").String() != ""
	}
}

func (b *apiKeyHealthBody) observeOversizedLine(prefix []byte) {
	// Response completion events can contain the entire generated output. Keep
	// the bounded prefix and inspect only structural fields present before it.
	b.unknownStream = true
	data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(prefix), []byte("data:")))
	typ := gjson.GetBytes(data, "type").String()
	b.failed = b.failed || typ == "error" || typ == "response.failed" || apiKeyHealthErrorJSON(data)
	b.terminal = b.terminal || typ == "response.completed" || typ == "message_stop" ||
		typ == "response.done" && gjson.GetBytes(data, "response.status").String() == "completed"
}

func (b *apiKeyHealthBody) finish() {
	if apiKeyHealthFailureStatus(b.status) {
		b.observation.record(false, b.status, fmtAPIKeyHealthStatus(b.status))
		return
	}
	if b.status < 200 || b.status >= 300 {
		return
	}
	upstreamTimeout := apiKeyHealthUpstreamTimeout(b.observation.ctx)
	if b.stream {
		if b.failed || upstreamTimeout || b.eof && !b.terminal && !b.unknownStream || b.readErr != nil && !errors.Is(b.readErr, context.Canceled) {
			b.observation.record(false, b.status, "Upstream stream failed or ended without a terminal event")
		} else if b.terminal {
			b.observation.record(true, b.status, "")
		}
		return
	}
	if apiKeyHealthErrorJSON(b.buf) || upstreamTimeout || b.readErr != nil && !errors.Is(b.readErr, context.Canceled) || b.eof && b.json && !b.oversized && !gjson.ValidBytes(b.buf) {
		b.observation.record(false, b.status, "Upstream response failed or was incomplete")
	} else if b.eof {
		b.observation.record(true, b.status, "")
	}
}

func fmtAPIKeyHealthStatus(status int) string {
	return "Upstream HTTP " + http.StatusText(status)
}

func (b *apiKeyHealthBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.finish()
	})
	return b.closeErr
}
