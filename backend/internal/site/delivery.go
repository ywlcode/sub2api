package site

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (m *Manager) peer(c *gin.Context) *Peer {
	if m.cfg.EffectiveMode() != Primary {
		c.AbortWithStatus(http.StatusNotFound)
		return nil
	}
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		for i := range m.cfg.Peers {
			p := &m.cfg.Peers[i]
			if subtle.ConstantTimeCompare([]byte(p.Token), []byte(token)) == 1 {
				return p
			}
		}
	}
	c.AbortWithStatus(http.StatusUnauthorized)
	return nil
}
func decodeSiteRequest(c *gin.Context, target any) error {
	var reader io.Reader = http.MaxBytesReader(c.Writer, c.Request.Body, maxSyncBody)
	if c.GetHeader("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if c.GetHeader("Content-Encoding") != "" {
		return errors.New("unsupported site encoding")
	}
	return decodeLimited(reader, target, maxSyncBody)
}
func (m *Manager) DeliveryHandler(c *gin.Context) {
	peer := m.peer(c)
	if peer == nil {
		return
	}
	store, ok := m.store.(DeliveryStore)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	var input SyncRequest
	if err := decodeSiteRequest(c, &input); err != nil || input.Protocol != ProtocolVersion || input.NodeID != peer.ID || input.Schema != m.store.Schema() || len(input.Events) > DeliveryBatch {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	output, err := store.Deliver(ctx, *peer, input.Events)
	if err != nil {
		slog.Warn("site event delivery failed", "node", peer.ID, "error", err)
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, output)
}

type siteTransfer struct {
	uploadJSON, uploadWire     int
	downloadJSON, downloadWire int64
}

func (m *Manager) postSite(ctx context.Context, path string, input *SyncRequest, output any) (siteTransfer, error) {
	var stats siteTransfer
	raw, err := marshalSyncRequest(input, maxSyncBody)
	if err != nil {
		return stats, err
	}
	packed, err := compress(raw)
	if err != nil {
		return stats, err
	}
	stats.uploadJSON, stats.uploadWire = len(raw), len(packed)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.cfg.PrimaryURL, "/")+path, bytes.NewReader(packed))
	if err != nil {
		return stats, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := m.client.Do(req)
	if err != nil {
		return stats, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("primary %s status %d", path, resp.StatusCode)
	}
	wire := &countingReader{r: io.LimitReader(resp.Body, maxSyncBody+1)}
	var reader io.Reader = wire
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, e := gzip.NewReader(wire)
		if e != nil {
			return stats, e
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	} else if resp.Header.Get("Content-Encoding") != "" {
		return stats, errors.New("unsupported synchronization encoding")
	}
	decoded := &countingReader{r: reader}
	err = decodeLimited(decoded, output, maxSyncBody)
	stats.downloadJSON, stats.downloadWire = decoded.n, wire.n
	return stats, err
}

func validateReceipts(ids []int64, accepted []int64, rejected []Rejection, complete bool) error {
	sent := make(map[int64]bool, len(ids))
	for _, id := range ids {
		sent[id] = true
	}
	seen := map[int64]bool{}
	check := func(id int64) error {
		if !sent[id] || seen[id] {
			return errors.New("invalid event acknowledgement")
		}
		seen[id] = true
		return nil
	}
	for _, id := range accepted {
		if err := check(id); err != nil {
			return err
		}
	}
	for _, r := range rejected {
		if r.Reason == "" {
			return errors.New("missing rejection reason")
		}
		if err := check(r.ID); err != nil {
			return err
		}
	}
	if complete && len(seen) != len(sent) {
		return errors.New("incomplete event acknowledgement")
	}
	return nil
}
func (m *Manager) deliverOnce(parent context.Context) (bool, error) {
	store, ok := m.store.(DeliveryStore)
	if !ok {
		return false, nil
	}
	m.deliveryGate.Lock()
	events, err := m.store.Pending(parent, DeliveryBatch)
	m.deliveryGate.Unlock()
	if err != nil || len(events) == 0 {
		return false, err
	}
	// An oversized diagnostic must not permanently block smaller events behind
	// it. Preserve the payload locally for reconciliation; do not pretend it was
	// imported on the primary or release a rejected billing reservation.
	kept := events[:0]
	var oversized []Rejection
	for _, event := range events {
		if len(event.Payload) > maxSyncBody-4096 {
			oversized = append(oversized, Rejection{ID: event.ID, Reason: "event exceeds upload body limit; retained locally for reconciliation"})
		} else {
			kept = append(kept, event)
		}
	}
	if len(oversized) > 0 {
		if err := store.MarkDelivered(parent, DeliveryResponse{Rejected: oversized}); err != nil {
			return false, err
		}
	}
	events = kept
	if len(events) == 0 {
		return true, nil
	}
	input := SyncRequest{Protocol: ProtocolVersion, NodeID: m.cfg.ID, Schema: m.store.Schema(), Events: events}
	var out DeliveryResponse
	_, err = m.postSite(parent, DeliveryPath, &input, &out)
	if err != nil {
		return false, err
	}
	if out.Protocol != ProtocolVersion || out.NodeID != m.cfg.ID {
		return false, errors.New("invalid delivery response")
	}
	ids := make([]int64, len(input.Events))
	for i, e := range input.Events {
		ids[i] = e.ID
	}
	if err = validateReceipts(ids, out.Acknowledged, out.Rejected, true); err != nil {
		return false, err
	}
	return true, store.MarkDelivered(parent, out)
}

// Snapshot capture prevents NEW upload attempts until installation completes.
// Already in-flight uploads have durably marked IDs and can finish concurrently:
// their receipt and balance are either both visible to the snapshot or neither.
func (m *Manager) run(ctx context.Context) {
	ticker := time.NewTicker(SyncInterval)
	defer ticker.Stop()
	for {
		if err := m.syncOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("site synchronization failed; lease was not renewed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *Manager) runDelivery(ctx context.Context) {
	for ctx.Err() == nil {
		active := false
		if m.identityVerified.Load() {
			uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			active, err = m.deliverOnce(uploadCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("site upload delayed; policy refresh remains independent", "error", err)
			}
		}
		if !active {
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func encodeSiteResponse(c *gin.Context, output SyncResponse) {
	raw, err := json.Marshal(output)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if len(raw) > maxSyncBody {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	packed, err := compress(raw)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Encoding", "gzip")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", packed)
}
