package site

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	syncConnectionPools = 5
	// Keep the existing per-round deadline independent of the pool count.
	// More standby pools must not make one round hold the delivery gate longer.
	syncAttemptsPerRound = 2
	syncAttemptTimeout   = 12 * time.Second
)

// Control requests are serial within each pool. HTTP/1 cancellation closes the
// underlying TCP connection, rather than only resetting an HTTP/2 stream on a
// stalled session. Model forwarding keeps its own transport and stream lifetime.
func newSiteControlTransport(base *http.Transport) *http.Transport {
	t := base.Clone()
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	// Clone can initialize the source transport's HTTP/2 ALPN advertisement.
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	t.MaxIdleConns, t.MaxIdleConnsPerHost, t.MaxConnsPerHost = 1, 1, 1
	return t
}

type siteHTTPError struct {
	path   string
	status int
}

func (e *siteHTTPError) Error() string {
	return fmt.Sprintf("primary %s status %d", e.path, e.status)
}

func retryableSiteSyncError(err error) bool {
	var status *siteHTTPError
	if errors.As(err, &status) {
		return status.status == http.StatusRequestTimeout || status.status == http.StatusTooManyRequests || status.status >= 500
	}
	var requestError *url.Error
	var networkError net.Error
	return errors.As(err, &requestError) || errors.As(err, &networkError) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Rotate independent keep-alive connections with normal heartbeats. Retry
// transport/transient failures within a bounded round; the next round continues
// at the next pool instead of restarting at the same failed connections.
// No parallel snapshots: installation and the request-start lease remain owned
// by syncOnce. A failed/partially decoded response is never installed or merged.
func (m *Manager) postSync(ctx context.Context, input *SyncRequest, output *SyncResponse) (total siteTransfer, err error) {
	for attempt := 0; attempt < syncAttemptsPerRound; attempt++ {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		index := m.syncNextClient
		client := m.syncClients[index]
		m.syncNextClient = (index + 1) % len(m.syncClients)
		timeout := syncAttemptTimeout
		if input.Version == "" {
			timeout = 90 * time.Second // retain the paged-bootstrap export allowance
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		var candidate SyncResponse
		var transfer siteTransfer
		transfer, err = m.postSite(attemptCtx, client, SyncPath, input, &candidate)
		cancel()
		total.uploadJSON += transfer.uploadJSON
		total.uploadWire += transfer.uploadWire
		total.downloadJSON += transfer.downloadJSON
		total.downloadWire += transfer.downloadWire
		if err == nil {
			*output = candidate
			return total, nil
		}
		if ctx.Err() != nil || !retryableSiteSyncError(err) {
			return total, err
		}
		client.CloseIdleConnections()
		if attempt+1 < syncAttemptsPerRound {
			slog.Warn("site synchronization retrying on alternate connection", "node", m.cfg.ID,
				"failed_pool", index+1, "next_pool", m.syncNextClient+1, "error", err)
		}
	}
	return total, err
}
