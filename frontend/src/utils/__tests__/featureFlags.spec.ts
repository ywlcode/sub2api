import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useAppStore } from '@/stores/app'
import { canViewPelicanShowcase, FeatureFlags, isFeatureFlagEnabled, makeSidebarFlag, resolveFeatureFlag } from '@/utils/featureFlags'
import type { PublicSettings } from '@/types'

vi.mock('@/api/admin/system', () => ({
  checkUpdates: vi.fn(),
}))

vi.mock('@/api/auth', () => ({
  getPublicSettings: vi.fn(),
}))

describe('FeatureFlags.subscription', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    delete (window as any).__APP_CONFIG__
  })

  it('reads subscription_enabled as an opt-out flag: visible before settings load', () => {
    expect(FeatureFlags.subscription.key).toBe('subscription_enabled')
    expect(FeatureFlags.subscription.mode).toBe('opt-out')
    expect(useAppStore().cachedPublicSettings).toBeNull()
    expect(isFeatureFlagEnabled(FeatureFlags.subscription)).toBe(true)
  })

  it('hides only when the backend explicitly sends false', () => {
    const store = useAppStore()
    const sidebarFlag = makeSidebarFlag(FeatureFlags.subscription)

    store.cachedPublicSettings = { subscription_enabled: false } as PublicSettings
    expect(sidebarFlag()).toBe(false)

    store.cachedPublicSettings = { subscription_enabled: true } as PublicSettings
    expect(sidebarFlag()).toBe(true)

    store.cachedPublicSettings = {} as PublicSettings
    expect(sidebarFlag()).toBe(true)
  })
})

describe('Pelican showcase audience', () => {
  it('keeps the gallery hidden until explicitly enabled, including for administrators', () => {
    expect(canViewPelicanShowcase(null, false)).toBe(false)
    expect(canViewPelicanShowcase({ pelican_showcase_enabled: false, pelican_showcase_admin_only: true }, true)).toBe(false)
  })

  it('preserves public access when the new audience setting is absent or off', () => {
    expect(canViewPelicanShowcase({ pelican_showcase_enabled: true }, false)).toBe(true)
    expect(canViewPelicanShowcase({ pelican_showcase_enabled: true, pelican_showcase_admin_only: false }, false)).toBe(true)
  })

  it('hides the same enabled gallery from regular users while retaining administrators', () => {
    const settings = { pelican_showcase_enabled: true, pelican_showcase_admin_only: true }
    expect(canViewPelicanShowcase(settings, false)).toBe(false)
    expect(canViewPelicanShowcase(settings, true)).toBe(true)
  })
})

describe('resolveFeatureFlag', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('reads an explicit boolean from the given settings object', () => {
    expect(resolveFeatureFlag({ subscription_enabled: false } as PublicSettings, FeatureFlags.subscription)).toBe(false)
    expect(resolveFeatureFlag({ subscription_enabled: true } as PublicSettings, FeatureFlags.subscription)).toBe(true)
    expect(resolveFeatureFlag({ available_channels_enabled: true } as PublicSettings, FeatureFlags.availableChannels)).toBe(true)
  })

  it('falls back to the declared mode when settings are missing or the key is absent', () => {
    expect(resolveFeatureFlag(undefined, FeatureFlags.subscription)).toBe(true)
    expect(resolveFeatureFlag(null, FeatureFlags.subscription)).toBe(true)
    expect(resolveFeatureFlag({} as PublicSettings, FeatureFlags.subscription)).toBe(true)
    expect(resolveFeatureFlag({} as PublicSettings, FeatureFlags.availableChannels)).toBe(false)
  })

  it('backs isFeatureFlagEnabled with the same resolution', () => {
    useAppStore().cachedPublicSettings = { subscription_enabled: false } as PublicSettings
    expect(isFeatureFlagEnabled(FeatureFlags.subscription)).toBe(false)
  })
})
