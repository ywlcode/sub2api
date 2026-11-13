package site

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type deliveryTestStore struct {
	testStore
	mu         sync.Mutex
	pending    []Event
	wait       chan struct{}
	started    chan struct{}
	applied    int
	incomplete bool
}

func (s *deliveryTestStore) PendingCoverage(context.Context) (bool, error) { return !s.incomplete, nil }

func (s *deliveryTestStore) ExportPage(context.Context, Peer, SyncRequest) (Snapshot, error) {
	return Snapshot{Schema: s.Schema(), Version: "v1", Full: true}, nil
}
func (s *deliveryTestStore) Pending(context.Context, int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.pending...), nil
}
func (s *deliveryTestStore) PendingIDs(context.Context) ([]int64, error) { return nil, nil }
func (s *deliveryTestStore) MarkDelivered(_ context.Context, r DeliveryResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
	return nil
}
func (s *deliveryTestStore) Deliver(ctx context.Context, p Peer, events []Event) (DeliveryResponse, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.wait:
	case <-ctx.Done():
		return DeliveryResponse{}, ctx.Err()
	}
	out := DeliveryResponse{Protocol: ProtocolVersion, NodeID: p.ID}
	for _, e := range events {
		out.Acknowledged = append(out.Acknowledged, e.ID)
	}
	return out, nil
}
func (s *deliveryTestStore) Install(context.Context, SyncResponse) error {
	s.mu.Lock()
	s.applied++
	s.mu.Unlock()
	return nil
}
func TestSiteSlowDeliveryDoesNotBlockLeaseRenewal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := strings.Repeat("s", 32)
	remote := &deliveryTestStore{wait: make(chan struct{}), started: make(chan struct{}, 1)}
	p, err := New(Config{Mode: Primary, ID: "main", Peers: []Peer{{ID: "office", Token: token}}}, remote, 1024)
	require.NoError(t, err)
	router := gin.New()
	router.POST(SyncPath, p.SyncHandler)
	router.POST(DeliveryPath, p.DeliveryHandler)
	server := httptest.NewServer(router)
	defer server.Close()
	local := &deliveryTestStore{pending: []Event{{ID: 1, Kind: "ops_error_logs", Payload: json.RawMessage(`{}`)}}}
	m, err := New(Config{Mode: Secondary, ID: "office", PrimaryURL: server.URL, Token: token, AllowHTTP: true}, local, 1024)
	require.NoError(t, err)
	defer m.Stop()
	local.incomplete = true
	require.ErrorContains(t, m.syncOnce(context.Background()), "legacy billing receipts")
	require.False(t, m.Ready())
	local.incomplete = false
	require.NoError(t, m.syncOnce(context.Background()))
	done := make(chan error, 1)
	go func() { _, err := m.deliverOnce(context.Background()); done <- err }()
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = m.syncOnce(ctx)
		cancel()
		require.NoError(t, err)
		require.True(t, m.Ready())
	}
	close(remote.wait)
	require.NoError(t, <-done)
	require.Empty(t, local.pending)
}
func TestSiteReceiptValidationIncludesRejections(t *testing.T) {
	require.NoError(t, validateReceipts([]int64{1, 2}, []int64{1}, []Rejection{{2, "deleted subscription"}}, true))
	for _, err := range []error{
		validateReceipts([]int64{1, 2}, []int64{1}, nil, true),
		validateReceipts([]int64{1}, []int64{1}, []Rejection{{1, "duplicate"}}, true),
		validateReceipts([]int64{1}, []int64{2}, nil, true),
		validateReceipts([]int64{1}, nil, []Rejection{{1, ""}}, true),
	} {
		require.Error(t, err)
	}
}
