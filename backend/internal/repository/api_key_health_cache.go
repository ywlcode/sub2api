package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Both keys share a Redis Cluster slot. State is retained until explicit reset:
// idle time alone must not erase escalation without a successful request.
const apiKeyHealthLuaState = `
local state, samples = KEYS[1], KEYS[2]
local clock = redis.call('TIME')
local now = tonumber(clock[1])
local function number(field)
    return tonumber(redis.call('HGET', state, field)) or 0
end
local function snapshot(tripped)
    local until_at = number('until')
    if until_at <= now then until_at = 0 end
    return {number('generation'), until_at, number('last_trip'), number('trips'),
        number('trip_failures'), number('trip_samples'), number('window'), number('threshold'),
        number('status'), redis.call('HGET', state, 'message') or '', tripped}
end
`

var apiKeyHealthBeginScript = redis.NewScript(apiKeyHealthLuaState + `
local signature = ARGV[1] .. ':' .. ARGV[2]
if redis.call('HGET', state, 'config') ~= signature then
    redis.call('HINCRBY', state, 'generation', 1)
    redis.call('HSET', state, 'config', signature, 'window', ARGV[1], 'threshold', ARGV[2], 'failures', 0)
    redis.call('DEL', samples)
end
return snapshot(0)
`)

var apiKeyHealthResultScript = redis.NewScript(apiKeyHealthLuaState + `
if number('generation') ~= tonumber(ARGV[1]) or number('until') > now then return {0} end
local success = ARGV[2] == '1'
local last_trip = number('last_trip')
if last_trip > 0 and now - last_trip >= 21600 and (success or number('has_success') == 1) then
    redis.call('HSET', state, 'trips', 0, 'last_trip', 0)
end
if success then redis.call('HSET', state, 'has_success', 1) end
redis.call('RPUSH', samples, ARGV[2])
local failures = number('failures')
if not success then failures = failures + 1 end
local count = redis.call('LLEN', samples)
if count > number('window') then
    if redis.call('LPOP', samples) == '0' then failures = failures - 1 end
    count = count - 1
end
redis.call('HSET', state, 'failures', failures)
if failures < number('threshold') then return {0} end
local trips = redis.call('HINCRBY', state, 'trips', 1)
local cooldown = 3600
if trips == 1 then cooldown = 900 elseif trips == 2 then cooldown = 1800 end
redis.call('HINCRBY', state, 'generation', 1)
redis.call('HSET', state, 'until', now + cooldown, 'last_trip', now, 'has_success', 0,
    'trip_failures', failures, 'trip_samples', count, 'status', ARGV[3], 'message', ARGV[4], 'failures', 0)
redis.call('DEL', samples)
return snapshot(1)
`)

var apiKeyHealthResetScript = redis.NewScript(apiKeyHealthLuaState + `
if redis.call('EXISTS', state) == 0 then return 0 end
redis.call('HINCRBY', state, 'generation', 1)
redis.call('HSET', state, 'until', 0, 'trips', 0, 'last_trip', 0, 'has_success', 0,
    'trip_failures', 0, 'trip_samples', 0, 'failures', 0, 'status', 0, 'message', '')
redis.call('DEL', samples)
return 1
`)

func apiKeyHealthKeys(accountID int64) []string {
	prefix := fmt.Sprintf("apikey_health:{%d}:", accountID)
	return []string{prefix + "state", prefix + "samples"}
}

func decodeAPIKeyHealthState(value any, err error) (*service.APIKeyHealthState, error) {
	if err != nil {
		return nil, err
	}
	values, ok := value.([]any)
	if !ok || len(values) != 1 && len(values) != 11 {
		return nil, fmt.Errorf("invalid API key health state result")
	}
	if len(values) == 1 {
		return nil, nil
	}
	numbers := make([]int64, 11)
	for i, value := range values {
		if i == 9 {
			continue
		}
		number, ok := value.(int64)
		if !ok {
			return nil, fmt.Errorf("invalid API key health state field %d", i)
		}
		numbers[i] = number
	}
	message, _ := values[9].(string)
	return &service.APIKeyHealthState{
		Generation: numbers[0], UntilUnix: numbers[1], TriggeredAtUnix: numbers[2],
		TripCount: int(numbers[3]), Failures: int(numbers[4]), Samples: int(numbers[5]),
		WindowSize: int(numbers[6]), FailureThreshold: int(numbers[7]), StatusCode: int(numbers[8]),
		Message: message, Tripped: numbers[10] == 1,
	}, nil
}

func (c *tempUnschedCache) BeginAPIKeyHealthAttempt(ctx context.Context, accountID int64, config service.APIKeyHealthConfig) (*service.APIKeyHealthState, error) {
	return decodeAPIKeyHealthState(apiKeyHealthBeginScript.Run(ctx, c.rdb, apiKeyHealthKeys(accountID), config.WindowSize, config.FailureThreshold).Result())
}

func (c *tempUnschedCache) RecordAPIKeyHealthResult(ctx context.Context, accountID, generation int64, success bool, statusCode int, message string) (*service.APIKeyHealthState, error) {
	value := "0"
	if success {
		value = "1"
	}
	return decodeAPIKeyHealthState(apiKeyHealthResultScript.Run(ctx, c.rdb, apiKeyHealthKeys(accountID), generation, value, statusCode, message).Result())
}

func (c *tempUnschedCache) ResetAPIKeyHealth(ctx context.Context, accountID int64) error {
	return apiKeyHealthResetScript.Run(ctx, c.rdb, apiKeyHealthKeys(accountID)).Err()
}
