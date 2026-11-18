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
	return traceSiteTransfer(req, "site event transfer finished", false, "events", events, "upload_wire_bytes", wireBytes)
}

func traceSiteTransfer(req *http.Request, message string, failuresOnly bool, fields ...any) (*http.Request, func(error)) {
	started := time.Now()
	var mu sync.Mutex
	phase, protocol, connection, remote := "connect", "", "", ""
	reused := false
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			phase, reused = "write_request", info.Reused
			connection = info.Conn.LocalAddr().String()
			remote = info.Conn.RemoteAddr().String()
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
		if failuresOnly && err == nil {
			return
		}
		mu.Lock()
		lastPhase, proto, conn, peer, wasReused := phase, protocol, connection, remote, reused
		mu.Unlock()
		result := "ok"
		if err != nil {
			result = "error"
		}
		attrs := append(append([]any(nil), fields...),
			"duration_ms", time.Since(started).Milliseconds(), "result", result, "phase", lastPhase,
			"protocol", proto, "connection", conn, "remote", peer, "connection_reused", wasReused)
		slog.Info(message, attrs...)
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), finish
}
