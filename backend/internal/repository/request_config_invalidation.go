package repository

import "context"

// Reuse the existing subscription rather than introduce a second Redis reader.
func (c *schedulerCache) NotifyRequestConfigInvalidation(ctx context.Context, message string) error {
	return c.rdb.Publish(ctx, authCacheInvalidateChannel, message).Err()
}
