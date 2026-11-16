package site

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Log transport progress, not payloads or credentials. A successful transfer
// only means the response was decoded; receipt validation and local persistence
// still follow in deliverOnce. Trace hooks may outlive the request on errors.
func traceEventUpload(req *http.Request, events, wireBytes int) (*http.Request, func(error)) {
	started := time.Now()
	var mu sync.Mutex
	phase, protocol, connection := "connect", "", ""
	reused := false
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			phase, reused = "write_request", info.Reused
			connection = info.Conn.LocalAddr().String()
			protocol = "HTTP/1.1"
			if conn, ok := info.Conn.(*tls.Conn); ok && conn.ConnectionState().NegotiatedProtocol == "h2" {
				protocol = "HTTP/2"
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if info.Err == nil && phase == "write_request" {
				phase = "wait_response"
			}
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			defer mu.Unlock()
			phase = "read_response"
		},
	}
	finish := func(err error) {
		mu.Lock()
		lastPhase, proto, conn, wasReused := phase, protocol, connection, reused
		mu.Unlock()
		result := "ok"
		if err != nil {
			result = "error"
		}
		slog.Info("site event transfer finished", "events", events, "upload_wire_bytes", wireBytes,
			"duration_ms", time.Since(started).Milliseconds(), "result", result, "phase", lastPhase,
			"protocol", proto, "connection", conn, "connection_reused", wasReused)
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), finish
}
