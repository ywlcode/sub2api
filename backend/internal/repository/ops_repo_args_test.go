//go:build unit

package repository

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsInsertErrorLogArgsPreservesExplicitZeroUpstreamStatus(t *testing.T) {
	zero := 0
	args := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{UpstreamStatusCode: &zero})

	require.Len(t, args, 40)
	encoded, ok := args[27].(sql.NullInt64)
	require.True(t, ok)
	require.True(t, encoded.Valid)
	require.Zero(t, encoded.Int64)
}

func TestOpsNullableIntPointerDistinguishesNilZeroAndStatus(t *testing.T) {
	missing := opsNullableIntPointer(nil).(sql.NullInt64)
	require.False(t, missing.Valid)

	zeroValue := 0
	zero := opsNullableIntPointer(&zeroValue).(sql.NullInt64)
	require.True(t, zero.Valid)
	require.Zero(t, zero.Int64)

	statusValue := 503
	status := opsNullableIntPointer(&statusValue).(sql.NullInt64)
	require.True(t, status.Valid)
	require.EqualValues(t, 503, status.Int64)
}

func TestOldSiteErrorCanOmitPublicMessage(t *testing.T) {
	row := map[string]any{}
	for _, field := range siteContractColumns["ops_error_logs"] {
		if field.Name != "client_error_message" {
			row[field.Name] = "test"
		}
	}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	normalized, err := normalizeSiteContractRow("ops_error_logs", raw)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(normalized, &row))
	value, exists := row["client_error_message"]
	require.True(t, exists)
	require.Nil(t, value)
}
