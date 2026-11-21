package server

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
)

// Keep site wiring separate from the main router so upstream router changes do
// not share an implementation block with the independent-site control plane.
func configureSites(r *gin.Engine, cfg *config.Config, lifecycle *Lifecycle, db *sql.DB, apiKeyService *service.APIKeyService, billingCache *service.BillingCacheService, channels *service.ChannelService, pricing *service.PricingService, errorPassthrough *service.ErrorPassthroughService, settingService *service.SettingService, ops *service.OpsService, handlers *handler.Handlers, cleanup *service.OpsCleanupService, groups service.GroupRepository) (gin.HandlerFunc, middleware2.ConnectionTraceRecorder, func()) {
	var siteIngress gin.HandlerFunc
	var siteRecorder middleware2.ConnectionTraceRecorder
	start := func() {}
	if cfg.Site.EffectiveMode() != site.Primary {
		// Embedded SPA fallback otherwise turns an absent control endpoint into
		// HTTP 200 HTML. Non-primary sites must explicitly reject every method.
		r.Any(site.SyncPath, func(c *gin.Context) { c.AbortWithStatus(404) })
		r.Any(site.DeliveryPath, func(c *gin.Context) { c.AbortWithStatus(404) })
	}
	if cfg.Site.EffectiveMode() != site.Standalone {
		if cfg.IsSecondary() {
			channels.UseSiteReplica()
		}
		store, err := repository.NewSiteStore(db, cfg)
		if err != nil {
			panic(fmt.Errorf("initialize site storage: %w", err))
		}
		if cleanup != nil {
			cleanup.SetSiteHistoryRetention(store)
		}
		store.AfterImport = func(ctx context.Context, events []service.UsageBillingEvent) error {
			return billingCache.InvalidateSiteUsage(ctx, events, apiKeyService)
		}
		store.MapChannelModel = func(id int64, model string) string {
			return channels.ResolveChannelMapping(context.Background(), id, model).MappedModel
		}
		store.ExportPricing = pricing.ExportSitePricing
		store.InstallPricing = pricing.InstallSitePricing
		store.AfterInstall = func(ctx context.Context) error { apiKeyService.InvalidateSiteReplica(); return nil }
		store.BeforeAuthCommit = apiKeyService.BeginSiteAuthInstall
		store.AfterPolicyInstall = func(ctx context.Context, changed map[string]bool) error {
			if cleanup != nil && (changed["$full"] || changed["settings"]) {
				if err := cleanup.Reload(ctx); err != nil {
					return err
				}
			}
			if changed["$full"] || changed["settings"] || changed["$policy"] {
				if err := settingService.RefreshSiteReplica(ctx); err != nil {
					return err
				}
				if ops != nil {
					if err := ops.RefreshRuntimeSettings(ctx); err != nil {
						return err
					}
				}
			}
			if changed["$full"] || changed["tls_fingerprint_profiles"] {
				if handlers != nil && handlers.Admin != nil {
					if err := handlers.Admin.TLSFingerprintProfile.RefreshSiteReplica(ctx); err != nil {
						return err
					}
				}
			}
			if changed["$full"] || changed["error_passthrough_rules"] {
				if err := errorPassthrough.RefreshSiteReplica(ctx); err != nil {
					return err
				}
			}
			channelChanged := changed["$full"] || changed["groups"]
			for table := range changed {
				if strings.HasPrefix(table, "channel") {
					channelChanged = true
				}
			}
			if channelChanged {
				return channels.RefreshSiteReplica(ctx)
			}
			return nil
		}
		if cfg.IsSecondary() {
			cfg.Site.Egress, err = site.NewEgressRoutes(site.EgressSettings{Processing: cfg.Site.ProcessingProxyURL, Forward: cfg.Site.ForwardProxyURL})
			if err != nil {
				panic(fmt.Errorf("initialize secondary egress: %w", err))
			}
		}
		sites, err := site.New(cfg.Site, store, cfg.RequestGateway().MaxBodySize)
		if err != nil {
			panic(err)
		}
		if cfg.Site.EffectiveMode() == site.Primary {
			sites.LoadPeerEgress = settingService.GetPeerEgress
			if handlers != nil && handlers.Gateway != nil && groups != nil {
				sites.LoadCatalog = func(ctx context.Context) (map[int64]site.CatalogGroup, error) {
					ids, err := store.DiscoveryGroupIDs(ctx)
					if err != nil {
						return nil, err
					}
					return buildSiteCatalog(ctx, ids, groups.GetByIDLite, handlers.Gateway.BuildSiteCatalog)
				}
				sites.AllowDiscovery = func(ctx context.Context) bool {
					// Active punishments and IP/Redis bans intentionally stay on
					// the primary. Disabling local discovery preserves that boundary.
					penalty, err := apiKeyService.SecurityPenaltyConfig(ctx)
					if err != nil || penalty.Enabled && penalty.RiskControlEnabled && !penalty.ObserveOnly {
						return false
					}
					if settingService.IPRisk != nil {
						ipPolicy, err := settingService.IPRisk.Config(ctx)
						if err != nil || ipPolicy.Enabled && !ipPolicy.ObserveOnly || ipPolicy.AnonymousAPI.Enabled {
							return false
						}
					}
					return true
				}
			}
			if handlers != nil && handlers.Admin != nil {
				handlers.Admin.SiteReconciliation = adminhandler.NewSiteReconciliationHandler(store)
			}
			r.POST(site.SyncPath, sites.SyncHandler)
			r.POST(site.DeliveryPath, sites.DeliveryHandler)
		}
		siteIngress = sites.Ingress()
		if cfg.IsSecondary() {
			siteRecorder = store
			lifecycle.checks = append(lifecycle.checks, func(context.Context) error {
				if !sites.Ready() {
					return fmt.Errorf("site replica expired")
				}
				return nil
			})
		}
		sites.BodyLimit = func() int64 { return cfg.RequestGateway().MaxBodySize }
		sites.BodyBudget = pkghttputil.NewBodyMemoryBudget(cfg.Gateway.RequestBodyMemoryBudgetBytes)
		// Router initialization registers settings callbacks. Do not let the
		// first synchronization race with those callback assignments.
		start = func() {
			sites.Start()
			if cfg.IsSecondary() && cleanup != nil {
				// Dedicated local histories share the daily scheduler. No global
				// aggregation, security audit or account maintenance runs here.
				cleanup.Start()
			}
		}
		lifecycle.onDrain = append(lifecycle.onDrain, sites.Stop)
	}
	return siteIngress, siteRecorder, start
}

// This runs on the primary's bounded catalog worker, outside the sync/usage
// transactions. Missing groups/variants naturally keep using primary discovery.
func buildSiteCatalog(ctx context.Context, ids []int64, get func(context.Context, int64) (*service.Group, error), render func(context.Context, *service.Group) (site.CatalogGroup, error)) (map[int64]site.CatalogGroup, error) {
	result := make(map[int64]site.CatalogGroup)
	remaining := 32 << 20
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		group, err := get(ctx, id)
		if err != nil || group == nil || !group.IsActive() {
			continue
		}
		entry, err := render(ctx, group)
		if err != nil {
			slog.Warn("site discovery group unavailable", "group", id, "error", err)
			continue
		}
		size := len(entry.Plain) + len(entry.Codex) + len(entry.Union) + len(entry.Policy)
		if size <= remaining && len(entry.Policy) > 0 && (len(entry.Plain)+len(entry.Codex)+len(entry.Union) > 0) {
			result[id] = entry
			remaining -= size
		}
	}
	return result, ctx.Err()
}
