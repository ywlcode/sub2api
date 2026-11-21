package site

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type catalogPageStore struct {
	deliveryTestStore
	page func(SyncRequest) (Snapshot, error)
}

func (s *catalogPageStore) ExportPage(_ context.Context, _ Peer, req SyncRequest) (Snapshot, error) {
	return s.page(req)
}

type catalogReceiptStore struct {
	testStore
	beforeInstall func()
}

func (*catalogReceiptStore) ReplicaID() string { return "4d979efa-ae15-4cd7-96a4-9b6ebcc4f84a" }
func (*catalogReceiptStore) PendingReceipts(context.Context) (map[int64]string, error) {
	return map[int64]string{9: strings.Repeat("a", 64)}, nil
}
func (s *catalogReceiptStore) Install(ctx context.Context, out SyncResponse) error {
	if s.beforeInstall != nil {
		s.beforeInstall()
	}
	return s.testStore.Install(ctx, out)
}

func catalogSites(t *testing.T, primaryStore, local Store) (*Manager, *Manager, *atomic.Int64) {
	t.Helper()
	office := Peer{ID: "office", Token: strings.Repeat("x", 32)}
	peers := []Peer{office, {ID: "other", Token: strings.Repeat("y", 32)}}
	p, err := New(Config{Mode: Primary, ID: "main", Peers: peers}, primaryStore, 1024)
	require.NoError(t, err)
	t.Cleanup(p.Stop)
	r := gin.New()
	r.POST(SyncPath, p.SyncHandler)
	r.POST(DeliveryPath, p.DeliveryHandler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	s, err := New(Config{Mode: Secondary, ID: office.ID, PrimaryURL: srv.URL, AllowHTTP: true, Token: office.Token}, local, 1024)
	require.NoError(t, err)
	t.Cleanup(s.Stop)
	clock := new(atomic.Int64)
	base := time.Now()
	p.now = func() time.Time { return base.Add(time.Duration(clock.Load())) }
	s.now = p.now
	return p, s, clock
}

func catalogRound(t *testing.T, p, s *Manager) {
	t.Helper()
	require.NoError(t, s.syncOnce(t.Context()))
	p.catalog.wg.Wait()
	s.catalog.wg.Wait()
}

func catalogPolicy(value string) []byte {
	h := sha256.Sum256([]byte(value))
	return h[:]
}

func TestCatalogNegotiatesOncePerHeartbeatAndDeduplicates(t *testing.T) {
	// Exercise the non-paged Store branch as well as a legacy v6 envelope.
	remote := &testStore{snapshot: Snapshot{Schema: "test-schema"}}
	p, s, clock := catalogSites(t, remote, &testStore{})
	shared, policy := []byte(`{"data":[]}`), catalogPolicy("public")
	var loads, gates, pulls atomic.Int32
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		loads.Add(1)
		return map[int64]CatalogGroup{
			1: {Plain: shared, Union: bytes.Clone(shared), Policy: policy},
			2: {Codex: shared, Policy: bytes.Clone(policy)},
			3: {Plain: shared}, // a group without a binding policy cannot be served
		}, nil
	}
	p.AllowDiscovery = func(context.Context) bool { gates.Add(1); return true }
	base := s.catalogClient.Transport
	s.catalogClient.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		pulls.Add(1)
		return base.RoundTrip(r)
	})
	input := SyncRequest{Protocol: ProtocolVersion, Schema: s.store.Schema(), NodeID: s.cfg.ID}
	var out SyncResponse
	_, err := s.postSite(t.Context(), s.syncClients[0], SyncPath, &input, &out)
	require.NoError(t, err)
	require.Empty(t, out.catalog)
	require.Zero(t, gates.Load())
	require.Zero(t, loads.Load())

	catalogRound(t, p, s)
	require.Nil(t, s.discoverySnapshot(), "the initiating heartbeat must finish before a cold build")
	require.True(t, s.Ready())
	catalogRound(t, p, s)
	view := s.discoverySnapshot()
	require.NotNil(t, view)
	require.Equal(t, shared, view.Get(1, CatalogUnion))
	require.Equal(t, shared, view.Get(2, CatalogCodex))
	require.Equal(t, policy, view.Get(2, CatalogPolicy))
	require.Same(t, &view.Get(1, CatalogPlain)[0], &view.Get(2, CatalogCodex)[0])
	require.Nil(t, view.Get(2, CatalogPlain))
	require.Nil(t, view.Get(3, CatalogPlain))
	require.Nil(t, view.Get(1, "unknown"))
	require.EqualValues(t, 3, pulls.Load(), "one manifest, one body, one binary policy")

	// Compare the actual heartbeat JSON with/without the HTTP extension.
	input.Version = s.version
	legacy, err := s.postSite(t.Context(), s.syncClients[0], SyncPath, &input, &out)
	require.NoError(t, err)
	input.catalog = view.download.root
	var reqHeader, respHeader string
	client := *s.syncClients[0]
	client.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		reqHeader = r.Header.Get(catalogHeader)
		resp, err := s.syncClients[0].Transport.RoundTrip(r)
		if err == nil {
			respHeader = resp.Header.Get(catalogHeader)
		}
		return resp, err
	})
	modern, err := s.postSite(t.Context(), &client, SyncPath, &input, &out)
	require.NoError(t, err)
	require.Equal(t, legacy, modern)
	require.Equal(t, input.catalog, reqHeader)
	require.Equal(t, "=", respHeader)
	t.Logf("steady heartbeat JSON %d/%d B, gzip %d/%d B (request/response); catalog header values %d/%d B",
		modern.uploadJSON, modern.downloadJSON, modern.uploadWire, modern.downloadWire, len(reqHeader), len(respHeader))
	before := gates.Load()
	for range 3 {
		clock.Add(int64(SyncInterval))
		catalogRound(t, p, s)
		require.Same(t, view, s.discoverySnapshot())
	}
	require.Equal(t, before+3, gates.Load(), "unchanged must recheck the live veto")
	require.EqualValues(t, 1, loads.Load())
	require.EqualValues(t, 3, pulls.Load())
	remote.snapshot.Rows = []Row{{Table: "users", Key: "1", Value: json.RawMessage(`{"balance":42}`)}}
	catalogRound(t, p, s)
	require.NotSame(t, view, s.discoverySnapshot(), "new auth version fences an in-flight request")
	require.Same(t, view.download, s.discoverySnapshot().download)
	require.EqualValues(t, 1, loads.Load(), "a debit does not rebuild the catalog")
	require.EqualValues(t, 3, pulls.Load())
}

func TestCatalogRefreshDeletesVariantsAndCannotRelabelOldBodies(t *testing.T) {
	remote := &testStore{snapshot: Snapshot{Schema: "test-schema"}}
	p, s, clock := catalogSites(t, remote, &testStore{})
	oldPolicy, newPolicy := catalogPolicy("broad"), catalogPolicy("narrow")
	oldGroups := map[int64]CatalogGroup{1: {Plain: []byte("broad-body"), Codex: []byte("old-codex"), Policy: oldPolicy}, 2: {Plain: []byte("gone"), Policy: oldPolicy}}
	newGroups := map[int64]CatalogGroup{1: {Plain: []byte("narrow-body"), Policy: newPolicy}}
	var loads atomic.Int32
	captured, release := make(chan struct{}), make(chan struct{})
	p.LoadCatalog = func(ctx context.Context) (map[int64]CatalogGroup, error) {
		switch loads.Add(1) {
		case 1:
			return oldGroups, nil
		case 2:
			close(captured) // a refresh has already read the old policy
			select {
			case <-release:
			case <-ctx.Done():
			}
			return oldGroups, ctx.Err()
		default:
			return newGroups, nil
		}
	}
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	old := s.discoverySnapshot()
	require.NotNil(t, old)
	clock.Add(int64(catalogRefresh))
	require.NoError(t, s.syncOnce(t.Context()))
	<-captured
	remote.snapshot.Rows = []Row{{Table: "groups", Key: "1", Value: json.RawMessage(`{"allowlist":["only"]}`)}}
	require.NoError(t, s.syncOnce(t.Context()))
	require.True(t, s.Ready())
	require.Nil(t, s.discoverySnapshot(), "a cached or in-flight old rendering cannot confirm the new config")
	close(release)
	p.catalog.wg.Wait()
	catalogRound(t, p, s)
	require.Nil(t, s.discoverySnapshot())
	clock.Add(int64(catalogMinRefresh))
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	view := s.discoverySnapshot()
	require.NotNil(t, view)
	require.Equal(t, newPolicy, view.Get(1, CatalogPolicy))
	require.Equal(t, "narrow-body", string(view.Get(1, CatalogPlain)))
	require.Nil(t, view.Get(1, CatalogCodex))
	require.Nil(t, view.Get(2, CatalogPolicy))
	require.Equal(t, oldPolicy, old.Get(1, CatalogPolicy))
	require.Equal(t, "broad-body", string(old.Get(1, CatalogPlain)))
	require.EqualValues(t, 3, loads.Load())
}

func TestCatalogVetoTimeoutDowngradeAndLostHeartbeat(t *testing.T) {
	remote := &testStore{snapshot: Snapshot{Schema: "test-schema"}}
	p, s, clock := catalogSites(t, remote, &testStore{})
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		return map[int64]CatalogGroup{1: {Plain: []byte("body"), Policy: catalogPolicy("1")}}, nil
	}
	var gate atomic.Int32
	release := make(chan struct{})
	defer close(release)
	var active atomic.Int32
	p.AllowDiscovery = func(context.Context) bool {
		switch gate.Load() {
		case 1:
			return false
		case 2:
			active.Add(1)
			defer active.Add(-1)
			<-release // a broken cache ignores cancellation and eventually returns true
		}
		return true
	}
	var headerMode atomic.Int32
	for _, client := range s.syncClients {
		base := client.Transport
		client.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
			mode := headerMode.Load()
			if mode == 3 {
				return nil, io.ErrUnexpectedEOF // no headers or authoritative reply
			}
			if mode == 1 {
				r.Header.Del(catalogHeader) // behaviour of an old primary
			}
			resp, err := base.RoundTrip(r)
			if err == nil && mode == 2 {
				resp.Header.Set(catalogHeader, "corrupt-root")
			}
			if err == nil && mode == 4 {
				resp.Header.Del(catalogHeader) // observed withdrawal, then a truncated response
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))
			}
			return resp, err
		})
	}
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	for _, mode := range []int32{1, 2} {
		require.NotNil(t, s.discoverySnapshot())
		headerMode.Store(mode)
		catalogRound(t, p, s)
		require.Nil(t, s.discoverySnapshot())
		require.True(t, s.Ready())
		headerMode.Store(0)
		catalogRound(t, p, s)
	}
	activeView := s.discoverySnapshot()
	expires := s.current.Load().expires
	headerMode.Store(3)
	require.Error(t, s.syncOnce(t.Context()))
	require.Same(t, activeView, s.discoverySnapshot())
	require.Equal(t, expires, s.current.Load().expires)
	clock.Add(int64(19 * time.Minute))
	require.Error(t, s.syncOnce(t.Context()))
	require.Same(t, activeView, s.discoverySnapshot())
	clock.Add(int64(time.Minute))
	require.Nil(t, s.discoverySnapshot())
	require.False(t, s.Ready())
	headerMode.Store(0)
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	require.NotNil(t, s.discoverySnapshot())
	headerMode.Store(4)
	require.Error(t, s.syncOnce(t.Context()))
	require.Nil(t, s.discoverySnapshot(), "transport failure must not undo a withdrawal already received")
	headerMode.Store(0)
	catalogRound(t, p, s)
	require.NotNil(t, s.discoverySnapshot())
	gate.Store(1)
	clock.Add(int64(SyncInterval))
	catalogRound(t, p, s)
	require.Nil(t, s.discoverySnapshot())
	require.True(t, s.current.Load().expires.After(expires))
	gate.Store(0)
	catalogRound(t, p, s)
	require.NotNil(t, s.discoverySnapshot())
	gate.Store(2)
	for range cap(p.catalog.gates) + 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := s.syncOnce(ctx)
		cancel()
		require.NoError(t, err)
		require.Nil(t, s.discoverySnapshot())
	}
	require.EqualValues(t, cap(p.catalog.gates), active.Load(), "uncooperative hooks have a fixed concurrency limit")
	require.True(t, s.Ready())
}

func TestCatalogInFlightWithdrawalKeepsBillingAndLeaseAvailable(t *testing.T) {
	remote := &catalogPageStore{page: func(req SyncRequest) (Snapshot, error) {
		return Snapshot{Version: "v1", Schema: "test-schema", Full: req.Version == ""}, nil
	}}
	remote.wait = make(chan struct{})
	close(remote.wait)
	local := &deliveryTestStore{pending: []Event{{ID: 7, Kind: "billing", Payload: json.RawMessage(`{}`)}}}
	p, s, clock := catalogSites(t, remote, local)
	policy := catalogPolicy("in-flight")
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		return map[int64]CatalogGroup{1: {Plain: []byte("data"), Policy: policy}}, nil
	}
	var veto atomic.Bool
	p.AllowDiscovery = func(context.Context) bool { return !veto.Load() }
	inFlight, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	base := s.catalogClient.Transport
	s.catalogClient.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && resp.ContentLength == int64(len(policy)) {
			close(inFlight)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return resp, err
	})
	catalogRound(t, p, s)
	require.NoError(t, s.syncOnce(t.Context()))
	select {
	case <-inFlight:
	case <-time.After(time.Second):
		t.Fatal("catalog transfer never reached the last object")
	}
	require.Nil(t, s.discoverySnapshot())
	oldLease := s.current.Load()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	active, err := s.deliverOnce(ctx)
	require.NoError(t, err)
	require.True(t, active)
	require.Empty(t, local.pending)
	veto.Store(true)
	clock.Add(int64(SyncInterval))
	require.NoError(t, s.syncOnce(ctx))
	s.catalog.wg.Wait()
	require.True(t, s.current.Load().expires.After(oldLease.expires))
	require.Nil(t, s.current.Load().catalog)
	require.Nil(t, s.discoverySnapshot())
	require.Empty(t, s.catalogCache.blobs, "partial content never enters the retained cache")
}

func TestCatalogPagedConfigurationReceiptsAndProgressivePull(t *testing.T) {
	proof := strings.Repeat("a", 64)
	var pages atomic.Int32
	local := &catalogReceiptStore{}
	remote := &catalogPageStore{page: func(req SyncRequest) (Snapshot, error) {
		pages.Add(1)
		snap := Snapshot{Version: "v1", Schema: "test-schema", Acknowledged: []int64{9}, ReceiptDigests: map[int64]string{9: proof}}
		if req.Version == "" {
			snap.Full = true
			snap.Rows = []Row{{Table: "groups", Key: "1", Value: json.RawMessage(`{}`)}}
			if req.Cursor == "" {
				snap.Cursor = "groups:1"
			} else if req.Cursor == "groups:1" && req.TargetVersion == "v1" {
				snap.Rows[0].Key = "2"
			} else {
				return Snapshot{}, errors.New("invalid config cursor")
			}
		}
		return snap, nil
	}}
	p, s, _ := catalogSites(t, remote, local)
	large := bytes.Repeat([]byte("m"), catalogChunkBytes*(catalogChunksPerRound+2)+11)
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		return map[int64]CatalogGroup{1: {Plain: large, Policy: catalogPolicy("1")}}, nil
	}
	var pulls, corrupt atomic.Int32
	base := s.catalogClient.Transport
	s.catalogClient.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		pulls.Add(1)
		resp, err := base.RoundTrip(r)
		if err == nil && resp.ContentLength > catalogChunkBytes {
			t.Errorf("oversized object chunk: %d", resp.ContentLength)
		}
		if err == nil && resp.ContentLength == 11 && corrupt.CompareAndSwap(0, 1) {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader("corruptdata"))
		}
		return resp, err
	})
	catalogRound(t, p, s)
	require.EqualValues(t, 2, pages.Load())
	require.Len(t, local.installed, 1)
	require.Len(t, local.installed[0].Rows, 2)
	require.Equal(t, proof, local.installed[0].ReceiptDigests[9])
	catalogRound(t, p, s)
	require.Nil(t, s.discoverySnapshot())
	require.EqualValues(t, catalogChunksPerRound, pulls.Load())
	for i := 0; i < 5 && s.discoverySnapshot() == nil; i++ {
		before := pulls.Load()
		catalogRound(t, p, s)
		require.LessOrEqual(t, pulls.Load()-before, int32(catalogChunksPerRound))
		require.True(t, s.Ready())
		if i == 0 {
			require.EqualValues(t, 1, corrupt.Load())
			require.Nil(t, s.discoverySnapshot(), "checksum mismatch must suppress the whole root")
		}
	}
	view := s.discoverySnapshot()
	require.NotNil(t, view)
	require.Equal(t, large, view.Get(1, CatalogPlain))
	require.Greater(t, pulls.Load(), int32(2*catalogChunksPerRound))
	require.Equal(t, []int64{9}, local.installed[len(local.installed)-1].Acknowledged)
}

func TestCatalogWithdrawalReusesOnlyVerifiedContentWithinPeer(t *testing.T) {
	local := &catalogReceiptStore{}
	proof := map[int64]string{9: strings.Repeat("a", 64)}
	var revision atomic.Int32
	remote := &catalogPageStore{page: func(req SyncRequest) (Snapshot, error) {
		version := strconv.Itoa(int(revision.Load()))
		out := Snapshot{Version: version, Full: req.Version == "", Schema: "test-schema", Acknowledged: []int64{9}, ReceiptDigests: proof}
		if req.Version != version {
			out.Rows = []Row{{Table: "groups", Key: "1", Value: json.RawMessage(`{}`)}}
		}
		return out, nil
	}}
	p, s, clock := catalogSites(t, remote, local)
	large := bytes.Repeat([]byte("x"), catalogChunkBytes+7)
	var veto atomic.Bool
	p.AllowDiscovery = func(context.Context) bool { return !veto.Load() }
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		policy := catalogPolicy(strconv.Itoa(int(revision.Load())))
		return map[int64]CatalogGroup{1: {Plain: large, Union: policy, Policy: policy}}, nil
	}
	var transferred atomic.Int64
	base := s.catalogClient.Transport
	s.catalogClient.Transport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && resp.StatusCode == http.StatusOK {
			part, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			transferred.Add(int64(len(part)))
			resp.Body = io.NopCloser(bytes.NewReader(part))
		}
		return resp, err
	})
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	old := s.discoverySnapshot()
	require.NotNil(t, old)
	require.Greater(t, transferred.Load(), int64(len(large)))
	veto.Store(true)
	local.beforeInstall = func() { require.Nil(t, s.discoverySnapshot(), "withdraw before auth install") }
	catalogRound(t, p, s)
	require.Nil(t, s.discoverySnapshot())
	require.NotEmpty(t, s.catalogCache.blobs)
	require.Equal(t, local.ReplicaID(), s.catalogCache.replica)
	revision.Add(1)
	clock.Add(int64(catalogMinRefresh))
	veto.Store(false)
	transferred.Store(0)
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	view := s.discoverySnapshot()
	require.NotNil(t, view)
	require.NotSame(t, old, view)
	require.Same(t, &old.Get(1, CatalogPlain)[0], &view.Get(1, CatalogPlain)[0])
	require.Equal(t, catalogPolicy("1"), view.Get(1, CatalogPolicy))
	require.Less(t, transferred.Load(), int64(1024), "only the manifest and the new policy should cross the wire")
	t.Logf("withdrawal + new root: %d B fetched, %d B body reused", transferred.Load(), len(large))
	local.beforeInstall = nil
	catalogRound(t, p, s)
	require.Empty(t, s.catalogCache.blobs)

	// An authenticated different peer, a replacement database and a guessed
	// object outside the manifest cannot borrow this publication.
	for _, tc := range []struct{ token, replica, digest string }{
		{p.cfg.Peers[1].Token, local.ReplicaID(), view.download.root},
		{s.cfg.Token, "", view.download.root},
		{s.cfg.Token, local.ReplicaID(), catalogDigest([]byte("foreign"))},
	} {
		raw, err := json.Marshal(catalogPull{Root: view.download.root, Digest: tc.digest, ReplicaID: tc.replica})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.cfg.PrimaryURL+SyncPath+"?"+catalogQuery, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		resp, err := s.catalogClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusGone, resp.StatusCode)
	}
	veto.Store(true)
	catalogRound(t, p, s)
	require.NotEmpty(t, s.catalogCache.blobs)
	clock.Add(int64(ReplicaTTL))
	catalogRound(t, p, s)
	require.Empty(t, s.catalogCache.blobs, "unreachable content ages out even with ongoing vetoed heartbeats")
}

func TestCatalogLeavesFull32MiBConfigurationBudget(t *testing.T) {
	remote := &catalogPageStore{}
	p, s, _ := catalogSites(t, remote, &testStore{})
	const version = "v1"
	row := Row{Table: "policy", Key: "1", Value: json.RawMessage(`""`)}
	out := SyncResponse{Protocol: ProtocolVersion, PrimaryID: p.cfg.ID, NodeID: s.cfg.ID, Schema: "test-schema", BaseVersion: version, Version: version, Rows: []Row{row}}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	row.Value = append(bytes.Repeat([]byte("a"), maxSyncBody-len(raw)+1), '"')
	row.Value[0] = '"'
	var nearLimit atomic.Bool
	remote.page = func(req SyncRequest) (Snapshot, error) {
		snap := Snapshot{Version: version, Full: req.Version == "", Schema: out.Schema}
		if nearLimit.Load() {
			snap.Rows = []Row{row}
		}
		return snap, nil
	}
	p.LoadCatalog = func(context.Context) (map[int64]CatalogGroup, error) {
		return map[int64]CatalogGroup{1: {Plain: []byte("catalog"), Policy: catalogPolicy("policy")}}, nil
	}
	catalogRound(t, p, s)
	catalogRound(t, p, s)
	nearLimit.Store(true)
	reqBody, err := json.Marshal(SyncRequest{NodeID: s.cfg.ID, Schema: out.Schema, Version: version, Protocol: ProtocolVersion})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.cfg.PrimaryURL+SyncPath, bytes.NewReader(reqBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set(catalogHeader, s.discoverySnapshot().download.root)
	resp, err := s.syncClients[0].Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "=", resp.Header.Get(catalogHeader))
	gz, err := gzip.NewReader(resp.Body)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()
	n, err := io.Copy(io.Discard, gz)
	require.NoError(t, err)
	require.EqualValues(t, maxSyncBody, n)
}
