//go:build integration

package repository

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestConnectionBodyBytesBackfillAndNetworkSeries(t *testing.T) {
	ctx := context.Background()
	schema := "bodybytes_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	dsn, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, migration := range []string{"294_request_connection_traces.sql", "320_ops_network_minutes.sql", "327_connection_trace_body_bytes.sql", "327_connection_trace_body_bytes.sql"} {
		raw, err := os.ReadFile("../../migrations/" + migration)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(raw))
		require.NoError(t, err)
	}
	repo := newUsageLogRepositoryWithSQL(nil, db)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	freshID := uuid.NewString()
	require.NoError(t, repo.writeConnectionTrace(ctx, service.ConnectionTrace{
		TraceID: freshID, StartedAt: at, Source: "application", Detail: requesttiming.Snapshot{BodyBytes: 12},
	}))
	// The hot graph must use the scalar even when legacy JSON differs.
	_, err = db.ExecContext(ctx, `UPDATE request_connection_traces SET detail='{"body_bytes":999}' WHERE trace_id=$1`, freshID)
	require.NoError(t, err)
	cases := []string{`{"body_bytes":0}`, `{"body_bytes":100}`, `{"body_bytes":1.0}`, `{}`, `{"body_bytes":"wrong"}`, `{"body_bytes":-1}`, `{"body_bytes":1.5}`, `{"body_bytes":9223372036854775808}`}
	for _, detail := range cases {
		_, err = db.ExecContext(ctx, `INSERT INTO request_connection_traces(trace_id,started_at,request_id,client_request_id,user_id,api_key_id,path,stream,detail)
  VALUES($1,$2,'','',0,0,'',false,$3::jsonb)`, uuid.NewString(), at, detail)
		require.NoError(t, err)
	}
	// Equal started_at values exercise the UUID tie cursor. At most two rows are
	// inspected per batch, and malformed rows do not stall later progress.
	done := false
	for batches := 0; !done && batches < 10; batches++ {
		done, err = repo.BackfillConnectionTraceBodyBytes(ctx, 2)
		require.NoError(t, err)
	}
	require.True(t, done)
	done, err = repo.BackfillConnectionTraceBodyBytes(ctx, 2)
	require.NoError(t, err)
	require.True(t, done)
	for _, record := range []service.ConnectionTrace{
		{TraceID: uuid.NewString(), StartedAt: at, Source: "nginx", Detail: requesttiming.Snapshot{BodyBytes: 1000}},
		{TraceID: uuid.NewString(), StartedAt: at.Add(time.Minute), Source: "application", Detail: requesttiming.Snapshot{BodyBytes: 1000}},
	} {
		require.NoError(t, repo.writeConnectionTrace(ctx, record))
	}
	series, err := readNetworkSeries(ctx, db, at, at.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, series.BodyPoints, 1)
	require.EqualValues(t, 113, series.BodyPoints[0].Bytes)
	require.EqualValues(t, 4, series.BodyPoints[0].Samples, "measured zero counts; unknown and invalid samples do not")
}
