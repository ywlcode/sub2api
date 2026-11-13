//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/stretchr/testify/require"
)

func TestSiteReplicationPreservesRequiredJSONNull(t *testing.T) {
	p := newSiteFollowupPair(t)
	ctx := context.Background()
	var groupID int64
	require.NoError(t, p.db.QueryRow(`SELECT group_id FROM api_keys WHERE id=$1`, p.key).Scan(&groupID))

	// Existing production rows can contain a JSON null in a NOT NULL JSONB
	// column. jsonb_populate_record turns that into SQL NULL unless installation
	// preserves the JSON value from the original projection.
	for i, value := range []string{`null`, `[]`, `["claude"]`, `null`} {
		_, err := p.db.Exec(`UPDATE groups SET supported_model_scopes=$1::jsonb WHERE id=$2`, value, groupID)
		require.NoError(t, err)
		if i == 0 {
			p.version = "" // exercise the initial full-snapshot path too
		}
		snapshot := p.sync(t)
		require.Equal(t, i == 0, snapshot.Full)
		var stored string
		var sqlNull bool
		require.NoError(t, p.replica.QueryRow(`SELECT supported_model_scopes::text,supported_model_scopes IS NULL FROM groups WHERE id=$1`, groupID).Scan(&stored, &sqlNull))
		require.False(t, sqlNull)
		require.JSONEq(t, value, stored)
	}

	// An absent required field must still reject the snapshot and roll back;
	// this is not permission to fill incomplete policy data with defaults.
	snapshot, err := p.primary.ExportPage(ctx, p.peer, site.SyncRequest{})
	require.NoError(t, err)
	for i := range snapshot.Rows {
		r := &snapshot.Rows[i]
		if r.Table == "groups" && r.Key == jsonNumber(groupID) {
			var row map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(r.Value, &row))
			delete(row, "supported_model_scopes")
			r.Value, err = json.Marshal(row)
			require.NoError(t, err)
		}
	}
	err = p.local.Install(ctx, site.SyncResponse{PrimaryID: "main", Full: true, Version: "must-not-install", Rows: snapshot.Rows})
	require.ErrorContains(t, err, `null value in column "supported_model_scopes"`)
	var version, stored string
	require.NoError(t, p.replica.QueryRow(`SELECT version FROM site_identity WHERE singleton`).Scan(&version))
	require.Equal(t, p.version, version)
	require.NoError(t, p.replica.QueryRow(`SELECT supported_model_scopes::text FROM groups WHERE id=$1`, groupID).Scan(&stored))
	require.Equal(t, "null", stored)
	require.Empty(t, p.sync(t).Rows, "the valid unchanged snapshot should still renew the lease")
}
