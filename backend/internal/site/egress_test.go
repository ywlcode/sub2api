package site

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRoleEgressRotationAndControlRecovery(t *testing.T) {
	const origin = "http://origin.invalid:9876"
	endpoint := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "origin.invalid:9876", r.Host)
			_, _ = io.WriteString(w, label)
		}))
	}
	one, two := endpoint("one"), endpoint("two")
	defer one.Close()
	defer two.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", Token: strings.Repeat("x", 32), AllowHTTP: true, PrimaryURL: origin, ForwardProxyURL: one.URL, ProcessingProxyURL: two.URL}, &deliveryTestStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	read := func(c *http.Client, ctx context.Context) string {
		req, err := http.NewRequestWithContext(ctx, "GET", origin, nil)
		require.NoError(t, err)
		res, err := c.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return string(b)
	}
	for _, c := range []*http.Client{m.syncClients[0], m.deliveryClient, {Transport: m.proxy.Transport}} {
		require.Equal(t, "one", read(c, context.Background()))
	}
	require.Equal(t, two.URL, m.cfg.Egress.Processing(""))
	require.NoError(t, m.cfg.Egress.Apply(EgressSettings{Forward: two.URL, Processing: one.URL}))
	for _, c := range []*http.Client{m.syncClients[0], m.deliveryClient, {Transport: m.proxy.Transport}} {
		require.Equal(t, "two", read(c, context.Background()))
	}
	require.Equal(t, one.URL, m.cfg.Egress.Processing(""))
	require.Equal(t, "one", read(m.syncClients[1], context.WithValue(context.Background(), bootstrapProxyKey{}, true)))
	require.Error(t, m.cfg.Egress.Apply(EgressSettings{Forward: "http://127.0.0.1:8888/invalid"}))
	require.Equal(t, "two", read(m.deliveryClient, context.Background()))
	two.Close()
	req, _ := http.NewRequest("GET", origin, nil)
	_, err = m.deliveryClient.Do(req)
	require.Error(t, err)
	require.NoError(t, m.cfg.Egress.Apply(EgressSettings{}))
	require.Equal(t, "one", read(m.deliveryClient, context.Background()))
}

func TestForwardProxyRotationKeepsLiveHTTP2Stream(t *testing.T) {
	release := make(chan struct{})
	once := sync.OnceFunc(func() { close(release) })
	defer once()
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		_, _ = io.WriteString(w, "headers\n")
		_ = http.NewResponseController(w).Flush()
		if r.URL.Path == "/stream" {
			<-release
		}
		_, _ = io.WriteString(w, "end")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	defer once()
	tunnel := func() (*httptest.Server, *atomic.Int32) {
		count := new(atomic.Int32)
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "CONNECT" {
				http.Error(w, "CONNECT only", 405)
				return
			}
			dst, err := net.Dial("tcp", origin.Listener.Addr().String())
			if err != nil {
				http.Error(w, "dial", 502)
				return
			}
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				dst.Close()
				return
			}
			count.Add(1)
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n\r\n")
			_ = buf.Flush()
			go func() { _, _ = io.Copy(dst, conn); dst.Close() }()
			_, _ = io.Copy(conn, dst)
			conn.Close()
		})), count
	}
	a, na := tunnel()
	defer a.Close()
	b, nb := tunnel()
	defer b.Close()
	m, err := New(Config{Mode: Secondary, ID: "office", Token: strings.Repeat("x", 32), PrimaryURL: origin.URL, ForwardProxyURL: a.URL}, &deliveryTestStore{}, 1<<20)
	require.NoError(t, err)
	defer m.Stop()
	roots := origin.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	m.forwardTransport.TLSClientConfig.RootCAs = roots
	build := m.forwardPool.build
	m.forwardPool.build = func(u *url.URL) (*http.Transport, error) {
		tr, err := build(u)
		if err == nil {
			tr.TLSClientConfig.RootCAs = roots
		}
		return tr, err
	}
	c := &http.Client{Transport: m.proxy.Transport, Timeout: 10 * time.Second}
	stream, err := c.Get(origin.URL + "/stream")
	require.NoError(t, err)
	defer stream.Body.Close()
	require.NoError(t, m.cfg.Egress.Apply(EgressSettings{Forward: b.URL}))
	resp, err := c.Get(origin.URL + "/next")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.EqualValues(t, 1, na.Load())
	require.EqualValues(t, 1, nb.Load())
	once()
	body, err := io.ReadAll(stream.Body)
	require.NoError(t, err)
	require.Equal(t, "headers\nend", string(body))
}
