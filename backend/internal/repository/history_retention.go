package repository

import (
	"context"
	"fmt"
	"time"
)

// Table/column/predicate are internal constants. Include tableoid because ctid
// is only unique within one physical table, including on partitioned histories.
func deleteHistoryBatches(ctx context.Context, db sqlExecutor, table, column, predicate string, cutoff time.Time) (int64, error) {
	const batchSize = 5000
	query := fmt.Sprintf(`WITH batch AS (
 SELECT tableoid,ctid FROM %s WHERE %s < $1 %s ORDER BY %s LIMIT $2
)
DELETE FROM %s WHERE (tableoid,ctid) IN (SELECT tableoid,ctid FROM batch)`, table, column, predicate, column, table)
	var total int64
	for {
		res, err := db.ExecContext(ctx, query, cutoff.UTC(), batchSize)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < batchSize {
			return total, nil
		}
	}
}
