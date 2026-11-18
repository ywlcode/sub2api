//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type compactTestDumper struct {
	mockDumper
	compactCalls int
}

func (d *compactTestDumper) DumpCompact(ctx context.Context) (io.ReadCloser, error) {
	d.compactCalls++
	return d.Dump(ctx)
}

func TestBackupCompactScheduleAndManualScope(t *testing.T) {
	ctx := context.Background()
	repo := newMockSettingRepo()
	seedS3Config(t, repo)
	dumper := &compactTestDumper{mockDumper: mockDumper{dumpData: []byte("compact SQL")}}
	svc := newTestBackupService(repo, dumper, newMockObjectStore())
	t.Cleanup(svc.Stop)
	cfg := archiveSchedule(time.Now().UTC().Day())
	cfg.Compact = true
	_, err := svc.UpdateSchedule(ctx, *cfg)
	require.NoError(t, err)
	loaded, err := svc.GetSchedule(ctx)
	require.NoError(t, err)
	require.True(t, loaded.Compact)
	svc.runScheduledBackup()
	require.Equal(t, 1, dumper.compactCalls)
	records, err := svc.ListBackups(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.True(t, records[0].Compact)
	require.Contains(t, records[0].FileName, "_compact.sql.gz")
	require.NotNil(t, records[0].MonthlyArchive)
	// A saved compact schedule never changes the explicit/manual full backup.
	record, err := svc.CreateBackup(ctx, "manual", 14)
	require.NoError(t, err)
	require.False(t, record.Compact)
	require.Equal(t, 1, dumper.compactCalls)
	cfg.Compact = false
	_, err = svc.UpdateSchedule(ctx, *cfg)
	require.NoError(t, err)
	svc.runScheduledBackup()
	require.Equal(t, 1, dumper.compactCalls)
}

func TestBackupCompactOldConfigAndUnsupportedDumper(t *testing.T) {
	var cfg BackupScheduleConfig
	require.NoError(t, json.Unmarshal([]byte(`{"enabled":true,"cron_expr":"0 2 * * *"}`), &cfg))
	require.False(t, cfg.Compact)
	repo := newMockSettingRepo()
	seedS3Config(t, repo)
	svc := newTestBackupService(repo, &mockDumper{}, newMockObjectStore())
	t.Cleanup(svc.Stop)
	_, err := svc.createBackup(context.Background(), "scheduled", 14, &BackupScheduleConfig{Compact: true})
	require.ErrorContains(t, err, "does not support compact backups")
	records, err := svc.ListBackups(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "failed", records[0].Status)
	require.True(t, records[0].Compact)
}
