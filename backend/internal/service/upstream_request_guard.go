package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// gatewayHTTPUpstream protects ordinary non-OpenAI HTTP forwarding independently
// of the health-breaker switch. OpenAI retains its own opt-in phase budgets.
// Keeping this outside the shared repository client excludes discovery, quota
// polling and other background traffic from client-disconnect policy.
func gatewayHTTPUpstream(upstream HTTPUpstream, limiter *RateLimitService, account *Account, cfg *config.Config) HTTPUpstream {
	upstream = apiKeyHealthHTTP(upstream, limiter, account)
	upstream = accountRequestGzipHTTP(upstream, account)
	if account == nil || account.Platform == PlatformOpenAI {
		return upstream
	}
	// A recognized Ark compaction can consume a response and retry internally.
	// Guard each physical attempt inside that adapter, so receiving headers
	// ends the header timer before the adapter waits for the summary body.
	return accountVolcengineHTTP(&guardedGatewayHTTP{HTTPUpstream: upstream, cfg: cfg}, account)
}

func (s *AntigravityGatewayService) guardedHTTPUpstream(account *Account) HTTPUpstream {
	var cfg *config.Config
	if s.settingService != nil {
		cfg = s.settingService.cfg
	}
	return gatewayHTTPUpstream(s.httpUpstream, s.rateLimitService, account, cfg)
}

type guardedGatewayHTTP struct {
	HTTPUpstream
	cfg *config.Config
}

func (u *guardedGatewayHTTP) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	return u.send(req, func(guarded *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.Do(guarded, proxyURL, accountID, concurrency)
	})
}

func (u *guardedGatewayHTTP) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req, func(guarded *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.DoWithTLS(guarded, proxyURL, accountID, concurrency, profile)
	})
}

func (u *guardedGatewayHTTP) send(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	req, guard := newUpstreamRequestGuard(req, u.cfg)
	handedOff := false
	defer func() {
		if !handedOff {
			guard.close()
		}
	}()
	if err := context.Cause(req.Context()); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := send(req)
	resp, err = guard.finish(req, resp, err)
	handedOff = err == nil && resp != nil && resp.Body != nil
	return resp, err
}

type upstreamRequestTimeout struct{ phase string }

func (e *upstreamRequestTimeout) Error() string   { return "Upstream " + e.phase + " timed out" }
func (e *upstreamRequestTimeout) Unwrap() error   { return context.DeadlineExceeded }
func (e *upstreamRequestTimeout) Timeout() bool   { return true }
func (e *upstreamRequestTimeout) Temporary() bool { return true }

type upstreamRequestGuard struct {
	ctx          context.Context
	cancel       context.CancelCauseFunc
	uploaded     chan struct{}
	response     chan struct{}
	stop         chan struct{}
	done         chan struct{}
	uploadOnce   sync.Once
	responseOnce sync.Once
	stopOnce     sync.Once
}

func newUpstreamRequestGuard(req *http.Request, cfg *config.Config) (*http.Request, *upstreamRequestGuard) {
	upload, headers, drain := 120*time.Second, 600*time.Second, 30*time.Second
	if cfg != nil {
		upload = time.Duration(cfg.Gateway.UpstreamUploadTimeoutSeconds) * time.Second
		headers = time.Duration(cfg.Gateway.ResponseHeaderTimeout) * time.Second
		drain = time.Duration(cfg.Gateway.UpstreamClientDisconnectGraceSeconds) * time.Second
	}
	// Detachment preserves the original caller for health attribution. Use the
	// same signal to stop abandoned uploads and bound post-response billing drains.
	caller := req.Context()
	if original, ok := caller.Value(apiKeyHealthCallerContextKey{}).(context.Context); ok && original != nil {
		caller = original
	}
	base := context.WithValue(req.Context(), apiKeyHealthCallerContextKey{}, caller)
	ctx, cancel := context.WithCancelCause(base)
	g := &upstreamRequestGuard{ctx: ctx, cancel: cancel, uploaded: make(chan struct{}), response: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	if caller.Err() != nil {
		cancel(context.Canceled)
	}
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			g.uploadOnce.Do(func() { close(g.uploaded) })
		}
	}}
	go g.watch(caller, upload, headers, drain)
	return req.Clone(httptrace.WithClientTrace(ctx, trace)), g
}

func (g *upstreamRequestGuard) watch(caller context.Context, upload, headers, drain time.Duration) {
	defer close(g.done)
	var phaseTimer, drainTimer *time.Timer
	var phaseCh, drainCh <-chan time.Time
	startPhase := func(timeout time.Duration) {
		if phaseTimer != nil {
			phaseTimer.Stop()
		}
		phaseCh = nil
		if timeout > 0 {
			phaseTimer = time.NewTimer(timeout)
			phaseCh = phaseTimer.C
		}
	}
	defer func() {
		if phaseTimer != nil {
			phaseTimer.Stop()
		}
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	phase := "upload"
	startPhase(upload)
	uploaded, response, disconnected := g.uploaded, g.response, caller.Done()
	for {
		select {
		case <-g.stop:
			return
		case <-g.ctx.Done():
			return
		case <-uploaded:
			uploaded = nil
			phase = "response headers"
			startPhase(headers)
		case <-response:
			// Early response headers also end the upload phase. Neither timer
			// limits a normal response body or a live long-running SSE stream.
			response, uploaded = nil, nil
			startPhase(0)
		case <-disconnected:
			disconnected = nil
			select {
			case <-g.response:
				if drain > 0 {
					drainTimer = time.NewTimer(drain)
					drainCh = drainTimer.C
					continue
				}
			default:
			}
			g.cancel(context.Canceled)
			return
		case <-drainCh:
			g.cancel(context.Canceled)
			return
		case <-phaseCh:
			// Prefer an already-delivered response/upload notification over a
			// timer selected at the same instant.
			select {
			case <-g.response:
				response, uploaded = nil, nil
				startPhase(0)
				continue
			default:
			}
			if phase == "upload" {
				select {
				case <-g.uploaded:
					uploaded = nil
					phase = "response headers"
					startPhase(headers)
					continue
				default:
				}
			}
			g.cancel(&upstreamRequestTimeout{phase: phase})
			return
		}
	}
}

func (g *upstreamRequestGuard) close() {
	g.stopOnce.Do(func() {
		close(g.stop)
		<-g.done
		g.cancel(context.Canceled)
	})
}

func (g *upstreamRequestGuard) finish(req *http.Request, resp *http.Response, err error) (*http.Response, error) {
	if resp != nil {
		g.responseOnce.Do(func() { close(g.response) })
	}
	if cause := context.Cause(g.ctx); cause != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		g.close()
		return nil, cause
	}
	if err != nil || resp == nil || resp.Body == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		g.close()
		return resp, err
	}
	if resp.Request == nil {
		resp.Request = req
	}
	resp.Body = &upstreamGuardBody{ReadCloser: resp.Body, guard: g}
	return resp, nil
}

type upstreamGuardBody struct {
	io.ReadCloser
	guard    *upstreamRequestGuard
	once     sync.Once
	closeErr error
}

func (b *upstreamGuardBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if cause := context.Cause(b.guard.ctx); cause != nil {
			err = cause
		}
		b.guard.close()
	}
	return n, err
}

func (b *upstreamGuardBody) Close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.guard.close()
	})
	return b.closeErr
}
