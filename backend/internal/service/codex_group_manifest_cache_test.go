package service

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGroupManifestCacheCoalescesAndOwnsBytes(t *testing.T) {
	var cache codexGroupManifestCache
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	key := [32]byte{1}
	build := func() ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte(`{"models":[]}`), nil
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := cache.build(context.Background(), key, build)
			if err != nil || string(body) != `{"models":[]}` {
				t.Error("invalid result", err)
			}
			body[0] = 'x'
		}()
	}
	<-started
	close(release)
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
	body, err := cache.build(context.Background(), key, build)
	require.NoError(t, err)
	require.Equal(t, `{"models":[]}`, string(body))
	cache.clear()
	_, err = cache.build(context.Background(), key, build)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestGroupManifestCacheExpiryBoundsAndInvalidationDuringBuild(t *testing.T) {
	var cache codexGroupManifestCache
	now := time.Now()
	for i := range groupManifestMaxEntries + 20 {
		var key [32]byte
		key[0] = byte(i)
		cache.store(key, []byte("body"), 0, now)
	}
	require.LessOrEqual(t, len(cache.entries), groupManifestMaxEntries)
	cache.clear()
	for i := range 30 {
		var key [32]byte
		key[0] = byte(i)
		cache.store(key, bytes.Repeat([]byte("x"), groupManifestMaxBody), 1, now)
	}
	require.LessOrEqual(t, cache.bytes, groupManifestMaxBytes)
	require.LessOrEqual(t, len(cache.entries), groupManifestMaxEntries)
	body, _ := cache.get([32]byte{29}, now.Add(groupManifestTTL))
	require.Nil(t, body)
	cache.store([32]byte{42}, []byte("old"), 0, now)
	body, _ = cache.get([32]byte{42}, now)
	require.Nil(t, body, "an invalidated in-flight build cannot refill the cache")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cache.build(ctx, [32]byte{}, func() ([]byte, error) { t.Fatal("canceled caller built manifest"); return nil, nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestGroupManifestFingerprintCoversPermissionsAndConfiguration(t *testing.T) {
	group := &Group{ID: 1, Platform: PlatformComposite}
	accounts := []Account{{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "deepseek-v4.1-flash"}}, Extra: map[string]any{}}}
	models := []string{"alias"}
	var routes []CompositeModelRoute
	fingerprint := func() [32]byte {
		key, err := groupManifestKey(group.Platform, models, accounts, group, routes, true)
		require.NoError(t, err)
		return key
	}
	previous := fingerprint()
	for name, change := range map[string]func(){
		"effective user models": func() { models = append(models, "another") },
		"model mapping":         func() { accounts[0].Credentials["model_mapping"] = map[string]any{"alias": "deepseek-v4-pro"} },
		"metadata":              func() { accounts[0].Extra["upstream_models"] = []string{"changed"} },
		"group restriction": func() {
			accounts[0].AccountGroups = []AccountGroup{{GroupID: 1, AllowedModels: []string{"restricted"}}}
		},
		"group routing": func() { group.ModelRouting = map[string][]int64{"alias": {2}}; group.ModelRoutingEnabled = true },
		"composite route": func() {
			routes = []CompositeModelRoute{{PublicModel: "alias", TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-6", Enabled: true}}
		},
		"shadow account": func() { parentID := int64(10); accounts[0].ParentAccountID = &parentID },
		"group identity": func() { group.ID = 2 },
		"platform":       func() { group.Platform = PlatformOpenAI },
	} {
		t.Run(name, func(t *testing.T) {
			change()
			next := fingerprint()
			require.NotEqual(t, previous, next)
			previous = next
		})
	}
	used := time.Now()
	accounts[0].LastUsedAt = &used
	require.Equal(t, previous, fingerprint(), "traffic timestamps must not defeat catalog caching")
}

func TestCachedGroupManifestMatchesBuilderAfterConfigurationChanges(t *testing.T) {
	group := &Group{ID: 1, Platform: PlatformDeepseek}
	accounts := []Account{{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "deepseek-chat"}}}}
	repo := &countingCodexModelsAccountRepo{accounts: accounts}
	service := &GatewayService{accountRepo: repo}
	for i, models := range [][]string{{"alias"}, {"alias"}, {"deepseek-chat"}, {"alias"}} {
		if i == 3 {
			accounts[0].Credentials["model_mapping"] = map[string]any{"alias": "deepseek-reasoner"}
		}
		got, err := service.BuildCodexModelsManifestForGroup(context.Background(), group, "", models)
		require.NoError(t, err)
		want, err := buildCodexModelsManifestForAccounts(group.Platform, models, accounts, group, nil, true)
		require.NoError(t, err)
		require.Equal(t, string(want), string(got))
	}
	require.Equal(t, int32(4), repo.calls.Load(), "configuration is still freshly read before using the rendering cache")
	require.Len(t, service.codexGroupManifestCache.entries, 3)
	service.InvalidateAvailableModelsCache(&group.ID, "")
	require.Empty(t, service.codexGroupManifestCache.entries)
}

func BenchmarkCodexGroupManifestCache(b *testing.B) {
	group := &Group{ID: 1, Platform: PlatformComposite}
	mapping := make(map[string]any)
	accounts := []Account{{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": mapping}}}
	models := make([]string, 50)
	for i := range models {
		models[i] = fmt.Sprintf("deepseek-alias-%d", i)
		mapping[models[i]] = "deepseek-chat"
	}
	svc := &GatewayService{}
	for _, mode := range []string{"before", "after"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if mode == "before" {
					_, err := buildCodexModelsManifestForAccounts(group.Platform, models, accounts, group, nil, true)
					if err != nil {
						b.Fatal(err)
					}
				} else {
					_, err := svc.cachedCodexGroupManifest(context.Background(), group.Platform, models, accounts, group, nil, true)
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func TestGroupManifestLargeCatalogIsCached(t *testing.T) {
	group := &Group{ID: 1, Platform: PlatformDeepseek}
	models := make([]string, 50)
	for i := range models {
		models[i] = fmt.Sprintf("deepseek-alias-%d", i)
	}
	svc := &GatewayService{}
	body, err := svc.cachedCodexGroupManifest(context.Background(), group.Platform, models, nil, group, nil, true)
	require.NoError(t, err)
	require.LessOrEqual(t, len(body), groupManifestMaxBody)
	require.Len(t, svc.codexGroupManifestCache.entries, 1)
	require.Equal(t, len(body), svc.codexGroupManifestCache.bytes)
	t.Logf("50-model catalog: %d bytes", len(body))
}
