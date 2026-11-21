// Secondary egress is chosen by the caller's role, independently of the target
// hostname. The transport itself keys persistent HTTP connections by proxy URL.
package site

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type EgressSettings struct {
	Processing string `json:"processing_proxy_url"`
	Forward    string `json:"forward_proxy_url"`
}

func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 1024 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("expected an HTTP(S) proxy origin with an explicit port and no credentials or path")
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port <= 0 || port > 65535 {
		return nil, errors.New("invalid proxy port")
	}
	return u, nil
}

func (s EgressSettings) Validate() error {
	if _, e := parseProxy(s.Processing); e != nil {
		return e
	}
	_, e := parseProxy(s.Forward)
	return e
}

type egressPair struct {
	processing string
	forward    *url.URL
}
type EgressRoutes struct {
	bootstrap egressPair
	active    atomic.Pointer[egressPair]
}
type bootstrapProxyKey struct{}

func NewEgressRoutes(defaults EgressSettings) (*EgressRoutes, error) {
	if e := defaults.Validate(); e != nil {
		return nil, e
	}
	forward, _ := parseProxy(defaults.Forward)
	r := &EgressRoutes{bootstrap: egressPair{defaults.Processing, forward}}
	r.active.Store(&r.bootstrap)
	return r, nil
}

func (r *EgressRoutes) Apply(s EgressSettings) error {
	if e := s.Validate(); e != nil {
		return e
	}
	p := r.bootstrap
	if s.Processing != "" {
		p.processing = s.Processing
	}
	if s.Forward != "" {
		p.forward, _ = parseProxy(s.Forward)
	}
	old := r.active.Load()
	if old.processing != p.processing || proxyAddress(old.forward) != proxyAddress(p.forward) {
		r.active.Store(&p)
	}
	return nil
}

func proxyAddress(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}
func (r *EgressRoutes) Processing(original string) string {
	if r != nil {
		if p := r.active.Load().processing; p != "" {
			return p
		}
	}
	return original
}
func (r *EgressRoutes) forwarding(fallback func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		proxy := r.active.Load().forward
		if req.Context().Value(bootstrapProxyKey{}) == true && r.bootstrap.forward != nil {
			proxy = r.bootstrap.forward
		}
		if proxy != nil {
			return proxy, nil
		}
		if fallback != nil {
			return fallback(req)
		}
		return nil, nil
	}
}

// The HTTP/2 transport can reuse by origin before evaluating Transport.Proxy.
// Separate pools on an egress change, even while an old SSE stream stays open.
type forwardEgressPool struct {
	mu      sync.Mutex
	routes  *EgressRoutes
	key     string
	current *http.Transport
	build   func(*url.URL) (*http.Transport, error)
}

func (p *forwardEgressPool) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	next := p.routes.active.Load().forward
	key := proxyAddress(next)
	if key != p.key {
		t, err := p.build(next)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.current.CloseIdleConnections()
		p.current, p.key = t, key
	}
	tr := p.current
	p.mu.Unlock()
	return tr.RoundTrip(req)
}
func (p *forwardEgressPool) closeIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current.CloseIdleConnections()
}
