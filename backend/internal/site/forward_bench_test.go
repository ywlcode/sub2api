package site

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Compare the phase guard with the same transport without the guard. Both use
// real TLS/H2 forwarding, ingress model inspection and gzip. At GOMAXPROCS=4,
// SetParallelism(50) supplies 200 concurrent clients. No paid upstream is used.
func BenchmarkSiteForward200(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, guarded := range []bool{false, true} {
		b.Run(map[bool]string{false: "connection_protection_only", true: "with_phase_guard"}[guarded], func(b *testing.B) {
			primary := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+strings.Repeat("output", 512)+"\n\n"+forwardCompleted)
			}))
			primary.EnableHTTP2 = true
			primary.StartTLS()
			defer primary.Close()
			m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: primary.URL, Token: strings.Repeat("s", 32)}, &deliveryTestStore{}, 1<<20)
			if err != nil {
				b.Fatal(err)
			}
			defer m.Stop()
			m.current.Store(&lease{expires: time.Now().Add(ReplicaTTL)})
			serverTransport, ok := primary.Client().Transport.(*http.Transport)
			if !ok {
				b.Fatal("test transport")
			}
			m.forwardTransport.TLSClientConfig.RootCAs = serverTransport.TLSClientConfig.RootCAs
			if !guarded {
				m.proxy.Transport = m.forwardTransport
			}
			router := gin.New()
			router.Use(m.Ingress())
			secondary := httptest.NewServer(router)
			defer secondary.Close()
			clientTransport := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256}
			defer clientTransport.CloseIdleConnections()
			client := &http.Client{Transport: clientTransport, Timeout: 30 * time.Second}
			body := []byte(`{"model":"gpt-test","stream":true,"input":"` + strings.Repeat("context for upload measurement ", 16000) + `"}`)
			var elapsed atomic.Int64
			b.SetParallelism(50)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					start := time.Now()
					resp, err := client.Post(secondary.URL+"/v1/responses", "application/json", bytes.NewReader(body))
					if err != nil {
						b.Error(err)
						return
					}
					result, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil || resp.StatusCode != http.StatusOK || !bytes.HasSuffix(result, []byte(forwardCompleted)) {
						b.Errorf("incomplete response: status=%d err=%v", resp.StatusCode, err)
						return
					}
					elapsed.Add(time.Since(start).Nanoseconds())
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(elapsed.Load())/float64(b.N)/1e6, "avg_ms")
		})
	}
}
