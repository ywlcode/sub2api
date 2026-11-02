export const API_KEY_HEALTH_CREDENTIAL_KEY = 'api_key_health_breaker'

export interface APIKeyHealthConfig {
  enabled: boolean
  window_size: number
  failure_threshold: number
}

export function defaultAPIKeyHealthConfig(): APIKeyHealthConfig {
  return { enabled: false, window_size: 100, failure_threshold: 10 }
}

export function readAPIKeyHealthConfig(credentials?: Record<string, unknown>): APIKeyHealthConfig {
  const value = credentials?.[API_KEY_HEALTH_CREDENTIAL_KEY]
  if (!value || typeof value !== 'object' || Array.isArray(value)) return defaultAPIKeyHealthConfig()
  const stored = value as Partial<APIKeyHealthConfig>
  const config = { ...defaultAPIKeyHealthConfig(), ...stored, enabled: stored.enabled === true }
  return validAPIKeyHealthConfig(config) ? config : defaultAPIKeyHealthConfig()
}

export function validAPIKeyHealthConfig(config: APIKeyHealthConfig): boolean {
  return Number.isInteger(config.window_size) && config.window_size >= 1 && config.window_size <= 10000 &&
    Number.isInteger(config.failure_threshold) && config.failure_threshold >= 1 && config.failure_threshold <= config.window_size
}

export function applyAPIKeyHealthConfig(credentials: Record<string, unknown>, config: APIKeyHealthConfig): boolean {
  if (config.enabled && !validAPIKeyHealthConfig(config)) return false
  credentials[API_KEY_HEALTH_CREDENTIAL_KEY] = config.enabled ? { ...config } : defaultAPIKeyHealthConfig()
  return true
}
