export const TRANSPORT_COOLDOWN_CREDENTIAL_KEY = 'upstream_transport_cooldown'

export interface TransportCooldownConfig {
  enabled: boolean
  duration_minutes: number
}

export function defaultTransportCooldownConfig(): TransportCooldownConfig {
  return { enabled: true, duration_minutes: 10 }
}

export function validTransportCooldownConfig(config: TransportCooldownConfig): boolean {
  return typeof config.enabled === 'boolean' && Number.isInteger(config.duration_minutes) &&
    config.duration_minutes >= 1 && config.duration_minutes <= 1440
}

export function readTransportCooldownConfig(credentials?: Record<string, unknown>): TransportCooldownConfig {
  const stored = credentials?.[TRANSPORT_COOLDOWN_CREDENTIAL_KEY]
  if (!stored || typeof stored !== 'object' || Array.isArray(stored)) return defaultTransportCooldownConfig()
  const value = stored as Partial<TransportCooldownConfig>
  const defaults = defaultTransportCooldownConfig()
  const config = {
    ...value,
    enabled: value.enabled ?? defaults.enabled,
    duration_minutes: value.duration_minutes ?? defaults.duration_minutes
  }
  return validTransportCooldownConfig(config) ? config : defaultTransportCooldownConfig()
}

export function applyTransportCooldownPatch(credentials: Record<string, unknown>, config: TransportCooldownConfig, current?: Record<string, unknown>): boolean {
  if (!validTransportCooldownConfig(config)) return false
  const previous = readTransportCooldownConfig(current)
  if (previous.enabled !== config.enabled || previous.duration_minutes !== config.duration_minutes) {
    credentials[TRANSPORT_COOLDOWN_CREDENTIAL_KEY] = { ...config }
  }
  return true
}
