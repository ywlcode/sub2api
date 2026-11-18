package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/lib/pq"
)

// Explicitly reviewed history tables, not a prefix such as ops_* or usage_*:
// those namespaces also contain settings, money and recovery state. Exclusions
// affect data only; schema, constraints and sequences remain in the backup.
var compactBackupHistoryTables = []string{
	"ops_error_logs", "ops_retry_attempts", "ops_system_metrics",
	"ops_metrics_hourly", "ops_metrics_daily", "ops_network_minutes",
	"ops_ingress_reject_aggregates", "ops_system_logs", "ops_system_log_cleanup_audits",
	"request_connection_traces", "request_timing_details",
	"request_capture_records", "request_capture_tasks",
	"user_traffic_events",
	"usage_request_phase_hourly_global", "usage_request_phase_hourly_dirty",
	"usage_billing_quotes",
}

// DumpCompact preserves all monetary state and hot/archive dedup claims. Only
// terminal billing events are omitted; pending/applied/blocked events must still
// recover after a restore. Both pg_dump and the filtered event COPY use the same
// exported snapshot, otherwise a concurrent debit could be lost or duplicated.
func (d *PgDumper) DumpCompact(ctx context.Context) (io.ReadCloser, error) {
	if d.db == nil {
		return nil, errors.New("acquire backup migration lock: nil sql db")
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err = pgAdvisoryLock(ctx, conn); err != nil {
		discardSQLConnection(conn)
		return nil, fmt.Errorf("acquire backup migration lock: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	// Reader.Close cancels subprocesses, then explicitly rolls back and unlocks
	// this session. Do not let reader cancellation race sql.Tx's auto-rollback.
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		cancel()
		return nil, errors.Join(err, releaseBackupMigrationLock(conn))
	}
	release := func() error {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return errors.Join(rollbackErr, releaseBackupMigrationLock(conn))
	}
	var snapshot string
	if err = tx.QueryRowContext(streamCtx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		cancel()
		return nil, errors.Join(err, release())
	}
	args, columns, err := compactBackupSelection(streamCtx, tx)
	if err != nil {
		cancel()
		return nil, errors.Join(err, release())
	}
	args = append([]string{"--no-owner", "--no-acl", "--clean", "--if-exists", "--encoding=UTF8", "--snapshot=" + snapshot}, args...)
	reader, writer := io.Pipe()
	// Context cancellation must also unblock a stalled consumer; killing a
	// subprocess alone cannot interrupt an io.Pipe write or exec's copy goroutine.
	stopCancel := context.AfterFunc(streamCtx, func() { _ = writer.CloseWithError(streamCtx.Err()) })
	result := &compactDumpReader{PipeReader: reader, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := d.writeCompactBackup(streamCtx, writer, args, snapshot, columns)
		err = errors.Join(err, release())
		stopCancel()
		result.err = err
		_ = writer.CloseWithError(err)
		cancel()
		close(result.done)
	}()
	return result, nil
}

func compactBackupSelection(ctx context.Context, tx *sql.Tx) ([]string, string, error) {
	tables := append(append([]string(nil), compactBackupHistoryTables...), "usage_billing_events")
	// Enumerate descendants rather than assuming partition names or requiring
	// a recent pg_dump's --exclude-table-data-and-children option.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE excluded(oid,has_data) AS (
		SELECT c.oid,c.relname='usage_billing_events' FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relname=ANY($1) AND c.relkind IN ('r','p')
		UNION SELECT i.inhrelid,e.has_data FROM pg_inherits i JOIN excluded e ON i.inhparent=e.oid
	) SELECT quote_ident(n.nspname)||'.'||quote_ident(c.relname),
	EXISTS(SELECT 1 FROM pg_constraint fk WHERE fk.contype='f' AND fk.confrelid=e.oid
	  AND NOT EXISTS(SELECT 1 FROM excluded source WHERE source.oid=fk.conrelid AND NOT source.has_data))
	FROM excluded e JOIN pg_class c ON c.oid=e.oid JOIN pg_namespace n ON n.oid=c.relnamespace
	ORDER BY 1`, pq.Array(tables))
	if err != nil {
		return nil, "", err
	}
	var args []string
	for rows.Next() {
		var table string
		var referenced bool
		if err = rows.Scan(&table, &referenced); err != nil {
			_ = rows.Close()
			return nil, "", err
		}
		if referenced {
			_ = rows.Close()
			return nil, "", fmt.Errorf("compact backup cannot omit %s: a retained table references it", table)
		}
		args = append(args, "--exclude-table-data="+table)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, "", err
	}
	// Generated columns are excluded exactly as pg_dump excludes them from COPY.
	// Missing billing schema is an error, never permission to omit recovery data.
	var columns string
	err = tx.QueryRowContext(ctx, `SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum)
		FROM pg_attribute WHERE attrelid='public.usage_billing_events'::regclass
		AND attnum>0 AND NOT attisdropped AND attgenerated=''`).Scan(&columns)
	return args, columns, err
}

func (d *PgDumper) writeCompactBackup(ctx context.Context, out io.Writer, args []string, snapshot, columns string) error {
	if _, err := io.WriteString(out, "-- Sub2API compact backup: diagnostic history omitted; billing dedup and recovery state retained.\n"); err != nil {
		return err
	}
	if err := d.runBackupCommand(ctx, out, "pg_dump", args...); err != nil {
		return err
	}
	// Billing events have no inbound foreign keys. Append their nonterminal rows
	// after pg_dump restores schema/data/constraints; their INSERT does not fire
	// the site's UPDATE-of-state outbox trigger. Do not disable any constraints.
	if _, err := fmt.Fprintf(out, "\nCOPY public.usage_billing_events (%s) FROM stdin;\n", columns); err != nil {
		return err
	}
	query := fmt.Sprintf(`BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
		SET TRANSACTION SNAPSHOT %s;
		SET LOCAL DateStyle='ISO'; SET LOCAL IntervalStyle='postgres'; SET LOCAL row_security=off;
		COPY (SELECT %s FROM public.usage_billing_events WHERE state NOT IN ('complete','legacy')) TO STDOUT;
		COMMIT;`, pq.QuoteLiteral(snapshot), columns)
	if err := d.runBackupCommand(ctx, out, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", query); err != nil {
		return err
	}
	_, err := io.WriteString(out, "\\.\n")
	return err
}

func (d *PgDumper) runBackupCommand(ctx context.Context, out io.Writer, name string, args ...string) error {
	base := []string{"-h", d.cfg.Host, "-p", fmt.Sprint(d.cfg.Port), "-U", d.cfg.User, "-d", d.cfg.DBName}
	commandContext := d.commandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	cmd := commandContext(ctx, name, append(base, args...)...)
	cmd.Env = append(cmd.Environ(), "PGCLIENTENCODING=UTF8")
	if d.cfg.Password != "" {
		cmd.Env = append(cmd.Environ(), "PGPASSWORD="+d.cfg.Password)
	}
	if d.cfg.SSLMode != "" {
		cmd.Env = append(cmd.Environ(), "PGSSLMODE="+d.cfg.SSLMode)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return nil
}

type compactDumpReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	once   sync.Once
}

func (r *compactDumpReader) Close() error {
	r.once.Do(func() {
		_ = r.PipeReader.Close()
		r.cancel()
		<-r.done
	})
	return r.err
}
