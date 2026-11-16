//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteReviewDeliveryMakesProgressBeforeDeadline(t *testing.T) {
	p := newSiteFollowupPair(t)
	var events []site.Event
	for i := 0; i < 70; i++ {
		events = append(events, p.bill(t))
	}
	// Stall the 65th bill on a real PostgreSQL lock. Earlier committed chunks
	// must survive a timeout and a lost HTTP acknowledgement.
	_, err := p.db.Exec(fmt.Sprintf(`CREATE FUNCTION review_block_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF NEW.event_id=%d THEN PERFORM pg_advisory_xact_lock(9148217); END IF; RETURN NEW; END $$;
CREATE TRIGGER review_block_receipt BEFORE INSERT ON site_receipts FOR EACH ROW EXECUTE FUNCTION review_block_receipt()`, events[64].ID))
	require.NoError(t, err)
	lock, err := p.db.Begin()
	require.NoError(t, err)
	defer lock.Rollback()
	_, err = lock.Exec(`SELECT pg_advisory_xact_lock(9148217)`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = p.primary.Deliver(ctx, p.peer, events)
	cancel()
	require.Error(t, err)
	var accepted int
	require.NoError(t, p.db.QueryRow(`SELECT count(*) FROM site_receipts`).Scan(&accepted))
	require.Equal(t, 64, accepted, "slow later events must not roll back all preceding bills")
	require.NoError(t, lock.Commit())
	out, err := p.primary.Deliver(context.Background(), p.peer, events)
	require.NoError(t, err)
	require.Len(t, out.Acknowledged, 70)
	p.balance(t, "30.00000000", "30.00000000")
	p.sync(t)
	p.balance(t, "30.00000000", "30.00000000")
}

func TestReviewHistoryDateBoundaryAcrossTimezones(t *testing.T) {
	db, _ := newBillingAuditDatabase(t)
	_, err := db.Exec(`CREATE TABLE review_dates(bucket_date date)`)
	require.NoError(t, err)
	for _, zone := range []string{"UTC", "America/Los_Angeles", "Asia/Shanghai"} {
		t.Run(zone, func(t *testing.T) {
			tx, err := db.Begin()
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.Exec(`SELECT set_config('TimeZone',$1,true)`, zone)
			require.NoError(t, err)
			_, err = tx.Exec(`INSERT INTO review_dates VALUES('2026-11-14'),('2026-11-15'),('2026-11-16')`)
			require.NoError(t, err)
			n, err := deleteHistoryBatches(context.Background(), tx, "review_dates", "bucket_date", "", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
		})
	}
}
