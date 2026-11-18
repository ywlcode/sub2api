import { describe, expect, it } from 'vitest'
import { applyTransportCooldownPatch, defaultTransportCooldownConfig, readTransportCooldownConfig } from '../transportCooldown'

describe('network pause compatibility', () => {
  it.each([
    [undefined, { enabled: true, duration_minutes: 10 }],
    [null, { enabled: true, duration_minutes: 10 }],
    [{ enabled: false }, { enabled: false, duration_minutes: 10 }],
    [{ enabled: false, duration_minutes: null }, { enabled: false, duration_minutes: 10 }],
    [{ enabled: null, duration_minutes: 3 }, { enabled: true, duration_minutes: 3 }],
    [{ enabled: 'false' }, { enabled: true, duration_minutes: 10 }],
    [{ duration_minutes: 0 }, { enabled: true, duration_minutes: 10 }]
  ])('matches backend defaults for %j', (stored, expected) => {
    expect(readTransportCooldownConfig({ upstream_transport_cooldown: stored })).toEqual(expected)
  })

  it('does not add credentials for an unchanged legacy account', () => {
    const credentials = { base_url: 'https://upstream.example' }
    expect(applyTransportCooldownPatch(credentials, defaultTransportCooldownConfig())).toBe(true)
    expect(credentials).toEqual({ base_url: 'https://upstream.example' })
  })

  it('explicitly restores the default when a previously disabled account is enabled', () => {
    const credentials = {}
    expect(applyTransportCooldownPatch(credentials, defaultTransportCooldownConfig(), {
      upstream_transport_cooldown: { enabled: false, duration_minutes: 10 }
    })).toBe(true)
    expect(credentials).toEqual({ upstream_transport_cooldown: { enabled: true, duration_minutes: 10 } })
  })
})
