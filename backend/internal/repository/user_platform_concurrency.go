package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Regional admission has a separate authority. A mode switch must not silently
// discard limits that were configured while Redis admission was active.
func checkRegionalPlatformConcurrency(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("regional concurrency requires a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var configured bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users u, LATERAL jsonb_each_text(COALESCE(NULLIF(u.platform_concurrency_limits, 'null'::jsonb), '{}'::jsonb)) p WHERE u.deleted_at IS NULL AND p.value::bigint > 0)`).Scan(&configured)
	if err != nil {
		return fmt.Errorf("check regional platform concurrency: %w", err)
	}
	if configured {
		return fmt.Errorf("regional concurrency does not support user platform limits; clear platform_concurrency_limits before enabling regional mode")
	}
	return nil
}

func userPlatformSlotKey(userID int64, platform string) string {
	return "concurrency:user_platform:" + strconv.FormatInt(userID, 10) + ":" + platform
}

// Live currently supports only OpenAI. Its existing user lease set is also the
// OpenAI live count, avoiding a second lease to refresh, transfer, or release.
var acquireUserPlatformScript = redis.NewScript(`
    redis.replicate_commands()
    local now = tonumber(redis.call('TIME')[1])
    local ttl = tonumber(ARGV[3])
    local id = ARGV[4]
    redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - ttl)
    redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now - 60)
    redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now - ttl)
    local exists = redis.call('ZSCORE', KEYS[1], id) ~= false
    local platformExists = redis.call('ZSCORE', KEYS[3], id) ~= false
    local liveCount = redis.call('ZCARD', KEYS[2])
    local total = redis.call('ZCARD', KEYS[1]) + liveCount
    local platformCount = redis.call('ZCARD', KEYS[3])
    if ARGV[5] == '1' then platformCount = platformCount + liveCount end
    if not exists and tonumber(ARGV[1]) > 0 and total >= tonumber(ARGV[1]) then return {0, now} end
    if not platformExists and tonumber(ARGV[2]) > 0 and platformCount >= tonumber(ARGV[2]) then return {0, now} end
    redis.call('ZADD', KEYS[1], now, id)
    redis.call('ZADD', KEYS[3], now, id)
    redis.call('EXPIRE', KEYS[1], ttl)
    redis.call('EXPIRE', KEYS[3], ttl)
    return {1, now}
`)

func (c *concurrencyCache) AcquireUserPlatformSlot(ctx context.Context, userID int64, platform string, total, platformMax int, requestID string) (bool, error) {
	live := 0
	if platform == service.PlatformOpenAI {
		live = 1
	}
	result, now, err := runScriptInt64Pair(ctx, c.rdb, acquireUserPlatformScript, []string{
		userSlotKey(userID), liveUserSlotKey(userID), userPlatformSlotKey(userID, platform),
	}, total, platformMax, c.slotTTLSeconds, requestID, live)
	if err == nil && result == 1 {
		c.touchActiveIndexAt(ctx, userActiveIndexKey, userID, now+int64(c.slotTTLSeconds))
	}
	return result == 1, err
}

func (c *concurrencyCache) ReleaseUserPlatformSlot(ctx context.Context, userID int64, platform, requestID string) error {
	pipe := c.rdb.TxPipeline()
	pipe.ZRem(ctx, userSlotKey(userID), requestID)
	pipe.ZRem(ctx, userPlatformSlotKey(userID, platform), requestID)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	c.refreshUserActiveIndex(ctx, userID)
	return nil
}
