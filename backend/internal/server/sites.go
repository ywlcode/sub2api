package server

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
)

// Keep site wiring separate from the main router so upstream router changes do
// not share an implementation block with the independent-site control plane.
func configureSites(r *gin.Engine, cfg *config.Config, lifecycle *Lifecycle, db *sql.DB, apiKeyService *service.APIKeyService, billingCache *service.BillingCacheService, channels *service.ChannelService, pricing *service.PricingService, errorPassthrough *service.ErrorPassthroughService, settingService *service.SettingService, ops *service.OpsService, handlers *handler.Handlers) (gin.HandlerFunc, middleware2.ConnectionTraceRecorder, func()) {
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
		store.AfterImport = func(ctx context.Context, events []service.UsageBillingEvent) error {
			return billingCache.InvalidateSiteUsage(ctx, events, apiKeyService)
		}
		store.MapChannelModel = func(id int64, model string) string {
			return channels.ResolveChannelMapping(context.Background(), id, model).MappedModel
		}
		store.ExportPricing = pricing.ExportSitePricing
		store.InstallPricing = pricing.InstallSitePricing
		store.AfterInstall = func(ctx context.Context) error { apiKeyService.InvalidateSiteReplica(); return nil }
		store.AfterPolicyInstall = func(ctx context.Context, changed map[string]bool) error {
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
		sites, err := site.New(cfg.Site, store, cfg.RequestGateway().MaxBodySize)
		if err != nil {
			panic(err)
		}
		if cfg.Site.EffectiveMode() == site.Primary {
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
		// Router initialization registers settings callbacks. Do not let the
		// first synchronization race with those callback assignments.
		start = sites.Start
		lifecycle.onDrain = append(lifecycle.onDrain, sites.Stop)
	}
	return siteIngress, siteRecorder, start
}
