package repository

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Opt-in real pg_dump/psql test. The documented runner provides a private Unix
// socket in a --network=none container, with no production endpoints or ports.
func TestCompactBackupPostgresRestore(t *testing.T) {
	socket := os.Getenv("SUB2API_BACKUP_TEST_SOCKET")
	if socket == "" {
		t.Skip("set SUB2API_BACKUP_TEST_SOCKET for the isolated compact backup restore test")
	}
	require.True(t, filepath.IsAbs(socket))
	require.True(t, strings.HasPrefix(filepath.Base(socket), "sub2api-compact-"))
	container := os.Getenv("SUB2API_BACKUP_TEST_CONTAINER")
	require.Regexp(t, `^sub2api-compact-backup-[A-Za-z0-9][A-Za-z0-9_.-]*$`, container)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := fmt.Sprintf("host=%s port=5432 user=postgres dbname=postgres sslmode=disable", socket)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	require.NoError(t, admin.PingContext(ctx))
	newDB := func() (*sql.DB, *PgDumper) {
		name := "compact_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		_, err := admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
		require.NoError(t, err)
		db, err := sql.Open("postgres", strings.Replace(dsn, "dbname=postgres", "dbname="+name, 1))
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = db.Close()
			_, dropErr := admin.ExecContext(context.Background(), "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
			require.NoError(t, dropErr)
		})
		return db, &PgDumper{db: db, cfg: &config.DatabaseConfig{Host: socket, Port: 5432, User: "postgres", DBName: name, SSLMode: "disable"},
			commandContext: func(ctx context.Context, binary string, args ...string) *exec.Cmd {
				require.Contains(t, []string{"pg_dump", "psql"}, binary)
				//nolint:gosec // G702: validated opt-in fixture container and fixed PostgreSQL tools; separate argv, never a shell.
				return exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", container, binary}, args...)...)
			}}
	}
	source, dumper := newDB()
	require.NoError(t, ApplyMigrations(ctx, source))
	_, err = source.ExecContext(ctx, "ALTER DATABASE "+pq.QuoteIdentifier(dumper.cfg.DBName)+" SET datestyle TO 'SQL, DMY'")
	require.NoError(t, err)
	var userID, keyID, accountID int64
	require.NoError(t, source.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,balance) VALUES('compact@example.test','hash',100) RETURNING id`).Scan(&userID))
	require.NoError(t, source.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,quota) VALUES($1,'sk-compact-test-only','compact',100) RETURNING id`, userID).Scan(&keyID))
	require.NoError(t, source.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES('compact','openai','apikey') RETURNING id`).Scan(&accountID))
	repo := &usageBillingRepository{db: source}
	commands := map[string]service.UsageBillingCommand{}
	usages := map[string]service.UsageLog{}
	quote := 1.0
	for _, state := range []string{"pending", "applied", "complete", "blocked", "legacy"} {
		cmd := service.UsageBillingCommand{RequestID: "compact-" + state, UserID: userID, APIKeyID: keyID, AccountID: accountID, BalanceCost: 1, APIKeyQuotaCost: 1, QuotedActualCost: &quote}
		usage := service.UsageLog{RequestID: cmd.RequestID, UserID: userID, APIKeyID: keyID, AccountID: accountID, Model: "test-模型\\literal\ttab\nline", InputTokens: 100, ActualCost: 1, TotalCost: 1, CreatedAt: time.Now().UTC()}
		require.NoError(t, repo.StageUsageBilling(ctx, &cmd, &usage))
		if state == "applied" || state == "complete" {
			_, err := repo.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: keyID})
			require.NoError(t, err)
		}
		if state == "complete" {
			require.NoError(t, repo.AcknowledgeUsageBillingCache(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: keyID}))
		}
		if state == "blocked" || state == "legacy" {
			_, err := source.ExecContext(ctx, "UPDATE usage_billing_events SET state=$1 WHERE request_id=$2", state, cmd.RequestID)
			require.NoError(t, err)
		}
		commands[state], usages[state] = cmd, usage
	}
	// Use both archive claims and live site receipts; neither may be omitted.
	archived := commands["complete"]
	archived.RequestID, archived.RequestFingerprint = "compact-archived", ""
	archived.Normalize()
	_, err = source.ExecContext(ctx, `INSERT INTO usage_billing_dedup_archive(request_id,api_key_id,request_fingerprint,created_at) VALUES($1,$2,$3,now());`, archived.RequestID, keyID, archived.RequestFingerprint)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO site_identity(mode,node_id) VALUES('primary','compact-primary');
		INSERT INTO site_receipts(node_id,event_id,digest) VALUES('secondary',42,'retained-receipt');
		INSERT INTO site_outbox(kind,payload) SELECT 'billing',payload FROM usage_billing_events WHERE state='complete';
		INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES(1,gen_random_uuid(),'{"text":"diagnostic history"}');
		CREATE TABLE compact_diagnostic_child () INHERITS (request_timing_details);
		INSERT INTO compact_diagnostic_child(usage_log_id,trace_id,detail) VALUES(2,gen_random_uuid(),'{"text":"child history"}');
		INSERT INTO request_capture_tasks(id,instance_id,created_at,data) VALUES('capture','test',now(),'{}');
		INSERT INTO request_capture_records(id,task_id,created_at,data) VALUES('capture-record','capture',now(),'{}')`)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `INSERT INTO user_traffic_hourly(bucket_start,user_id,api_key_id,group_id,upload_bytes,download_bytes,requests) VALUES(now(),$1,$2,0,100,200,1)`, userID, keyID)
	require.NoError(t, err)

	// Pause after the snapshot is exported but before pg_dump imports it. A live
	// debit now changes balances, claims and events together, but must not leak
	// into either half of the earlier backup snapshot.
	baseCommand := dumper.commandContext
	ready, proceed := make(chan struct{}), make(chan struct{})
	dumper.commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "pg_dump" {
			close(ready)
			select {
			case <-proceed:
			case <-ctx.Done():
			}
		}
		return baseCommand(ctx, name, args...)
	}
	r, err := dumper.DumpCompact(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	type readResult struct {
		data []byte
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		closeErr := r.Close()
		if err == nil {
			err = closeErr
		}
		readDone <- readResult{data, err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pendingKey := service.UsageBillingEventKey{RequestID: commands["pending"].RequestID, APIKeyID: keyID}
	_, err = repo.ApplyStagedUsageBilling(ctx, pendingKey)
	require.NoError(t, err)
	close(proceed)
	var dumped readResult
	select {
	case dumped = <-readDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, dumped.err)
	dumper.commandContext = baseCommand
	target, restorer := newDB()
	require.NoError(t, restorer.Restore(ctx, bytes.NewReader(dumped.data)))
	count := func(db *sql.DB, query string) int64 {
		var n int64
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&n))
		return n
	}
	balance := func(db *sql.DB) string {
		var b string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", userID).Scan(&b))
		return b
	}
	require.Equal(t, "97.00000000", balance(source))
	require.Equal(t, "98.00000000", balance(target), "snapshot must precede the concurrent debit")
	require.EqualValues(t, 2, count(target, "SELECT count(*) FROM usage_logs"))
	require.EqualValues(t, 2, count(target, "SELECT count(*) FROM usage_billing_dedup"))
	require.EqualValues(t, 1, count(target, "SELECT count(*) FROM usage_billing_dedup_archive"))
	require.EqualValues(t, 3, count(target, "SELECT count(*) FROM usage_billing_events"))
	require.EqualValues(t, 1, count(target, "SELECT count(*) FROM usage_billing_events WHERE state='pending'"))
	var observed time.Time
	require.NoError(t, target.QueryRowContext(ctx, "SELECT observed_at FROM usage_billing_events WHERE state='pending'").Scan(&observed))
	require.WithinDuration(t, usages["pending"].CreatedAt, observed, time.Microsecond)
	for _, table := range []string{"usage_billing_quotes", "request_timing_details", "compact_diagnostic_child", "request_capture_tasks", "request_capture_records"} {
		require.Zero(t, count(target, "SELECT count(*) FROM "+pq.QuoteIdentifier(table)), table)
	}
	for _, table := range []string{"site_identity", "site_outbox", "site_receipts", "user_traffic_hourly"} {
		require.EqualValues(t, 1, count(target, "SELECT count(*) FROM "+pq.QuoteIdentifier(table)), table)
	}
	restored := &usageBillingRepository{db: target}
	for _, state := range []string{"pending", "pending", "applied"} {
		cmd := commands[state]
		_, err := restored.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: keyID})
		require.NoError(t, err)
	}
	// Completed event payloads were omitted, but re-delivery still cannot debit
	// twice because both hot and archived idempotency claims survived.
	cmd, usage := commands["complete"], usages["complete"]
	require.NoError(t, restored.StageUsageBilling(ctx, &cmd, &usage))
	_, err = restored.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: cmd.RequestID, APIKeyID: keyID})
	require.NoError(t, err)
	applied, err := restored.Apply(ctx, &archived)
	require.NoError(t, err)
	require.False(t, applied.Applied)
	require.Equal(t, "97.00000000", balance(target))
	require.EqualValues(t, 3, count(target, "SELECT count(*) FROM usage_logs"))
	var restoredModel string
	require.NoError(t, target.QueryRowContext(ctx, "SELECT model FROM usage_logs WHERE request_id=$1", pendingKey.RequestID).Scan(&restoredModel))
	require.Equal(t, usages["pending"].Model, restoredModel)
	_, err = restored.ApplyStagedUsageBilling(ctx, service.UsageBillingEventKey{RequestID: commands["blocked"].RequestID, APIKeyID: keyID})
	require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)

	// Full backup stays unchanged, including diagnostics and completed journals.
	fullReader, err := dumper.Dump(ctx)
	require.NoError(t, err)
	fullData, err := io.ReadAll(fullReader)
	require.NoError(t, err)
	require.NoError(t, fullReader.Close())
	fullDB, fullRestorer := newDB()
	require.NoError(t, fullRestorer.Restore(ctx, bytes.NewReader(fullData)))
	require.EqualValues(t, 5, count(fullDB, "SELECT count(*) FROM usage_billing_events"))
	require.EqualValues(t, 2, count(fullDB, "SELECT count(*) FROM request_timing_details"))
	require.EqualValues(t, 3, count(fullDB, "SELECT count(*) FROM usage_billing_quotes"))
	// Any restore SQL error must fail and roll back earlier statements.
	require.Error(t, restorer.Restore(ctx, strings.NewReader("UPDATE users SET balance=0; SELECT compact_test_missing_function();")))
	require.Equal(t, "97.00000000", balance(target))

	// A future schema change must not silently create unrestorable compact dumps.
	observer, err := source.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, observer.Close()) }()
	assertUnlocked := func() {
		var acquired bool
		require.NoError(t, observer.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationsAdvisoryLockID).Scan(&acquired))
		require.True(t, acquired)
		_, err := observer.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationsAdvisoryLockID)
		require.NoError(t, err)
	}
	_, err = source.ExecContext(ctx, `CREATE TABLE compact_retained_reference(id text REFERENCES request_capture_tasks(id))`)
	require.NoError(t, err)
	rejected, err := dumper.DumpCompact(ctx)
	require.ErrorContains(t, err, "a retained table references it")
	require.Nil(t, rejected)
	assertUnlocked()
	_, err = source.ExecContext(ctx, "DROP TABLE compact_retained_reference")
	require.NoError(t, err)
	// Closing a partially consumed stream must cancel its subprocess and release
	// the real snapshot/session lock without waiting for a 30-second command.
	dumper.commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}
	canceled, err := dumper.DumpCompact(ctx)
	require.NoError(t, err)
	_, err = bufio.NewReader(canceled).ReadString('\n')
	require.NoError(t, err)
	started := time.Now()
	require.Error(t, canceled.Close())
	require.Less(t, time.Since(started), 5*time.Second)
	assertUnlocked()
	// A canceled job must also release its snapshot if nobody reads or closes
	// the stream (e.g. its destination stops consuming before the first byte).
	stalledCtx, stop := context.WithCancel(ctx)
	stalled, err := dumper.DumpCompact(stalledCtx)
	require.NoError(t, err)
	stalledReader, ok := stalled.(*compactDumpReader)
	require.True(t, ok)
	stop()
	select {
	case <-stalledReader.done:
	case <-time.After(5 * time.Second):
		_ = stalled.Close()
		t.Fatal("canceled compact dump retained a stalled pipe/snapshot")
	}
	require.Error(t, stalled.Close())
	assertUnlocked()
}
