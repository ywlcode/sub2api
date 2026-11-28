package service

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const upstreamPromptCacheShadowPolicyTTL = 5 * time.Second

type upstreamPromptCacheShadowSnapshot struct {
	loadedAt          time.Time
	generation        uint64
	channelsByAccount map[int64]int64
}

// This is independent of model pricing and gateway scheduling. An unavailable
// policy disables diagnostics only; it must never reject a user's request.
type upstreamPromptCacheShadowPolicy struct {
	snapshot   atomic.Pointer[upstreamPromptCacheShadowSnapshot]
	generation atomic.Uint64
	load       singleflight.Group
}

func (p *upstreamPromptCacheShadowPolicy) invalidate() {
	p.generation.Add(1)
	p.snapshot.Store(nil)
	p.load.Forget("policy")
}

func (p *upstreamPromptCacheShadowPolicy) fresh() *upstreamPromptCacheShadowSnapshot {
	v := p.snapshot.Load()
	if v != nil && v.generation == p.generation.Load() && time.Since(v.loadedAt) < upstreamPromptCacheShadowPolicyTTL {
		return v
	}
	return nil
}

// PromptCacheShadowChannelForAccount returns zero for disabled or unbound
// accounts. Hot requests read one immutable snapshot, never one SQL per account.
func (s *ChannelService) PromptCacheShadowChannelForAccount(ctx context.Context, accountID int64) int64 {
	if s == nil || accountID < 1 || s.siteReplica.Load() {
		return 0
	}
	p := &s.promptCacheShadowPolicy
	if v := p.fresh(); v != nil {
		return v.channelsByAccount[accountID]
	}
	r, ok := s.repo.(UpstreamChannelPromptCacheShadowRepository)
	if !ok {
		return 0
	}
	value, _, _ := p.load.Do("policy", func() (any, error) {
		if v := p.fresh(); v != nil {
			return v, nil
		}
		generation := p.generation.Load()
		// Only the cache refresh may wait for SQL. Diagnostic failures are
		// negatively cached too, so an outage cannot flood the database.
		queryCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		accounts, err := r.ListPromptCacheShadowAccounts(queryCtx)
		v := &upstreamPromptCacheShadowSnapshot{loadedAt: time.Now(), generation: generation, channelsByAccount: make(map[int64]int64, len(accounts))}
		if err != nil {
			slog.Warn("prompt cache channel policy unavailable; observation skipped", "error", err)
		} else {
			for _, a := range accounts {
				if a.AccountID > 0 && a.ChannelID > 0 {
					v.channelsByAccount[a.AccountID] = a.ChannelID
				}
			}
		}
		if generation == p.generation.Load() {
			p.snapshot.Store(v)
		}
		return v, nil
	})
	v, ok := value.(*upstreamPromptCacheShadowSnapshot)
	if !ok || v.generation != p.generation.Load() {
		return 0
	}
	return v.channelsByAccount[accountID]
}

// Admin writes take effect on this process immediately. Existing Redis channel
// notifications invalidate other processes; TTL also covers automatic binding
// updates and missed notifications without changing model-pricing settings.
func (s *ChannelService) InvalidatePromptCacheShadowPolicy() {
	if s == nil {
		return
	}
	s.promptCacheShadowPolicy.invalidate()
	s.notifyCacheUpdate()
}
