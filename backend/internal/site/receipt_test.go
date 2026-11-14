package site

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEventDigestMatchesLegacyHTTPWireWithoutLosingPrecision(t *testing.T) {
	for _, payload := range []string{
		`{ "amount": 0.123456789012345678, "id": 9007199254740993 }`,
		"{\"message\":\"<>&\u2028\u2029中文\"}",
		`{"message":"\u003c\u0026\u2028","amount":1.00000000}`,
	} {
		e := Event{ID: 1, Kind: "billing", Payload: json.RawMessage(payload)}
		wire, err := json.Marshal(e)
		require.NoError(t, err)
		var decoded Event
		require.NoError(t, json.Unmarshal(wire, &decoded))
		legacy := sha256.Sum256(append([]byte(e.Kind+"\x00"), decoded.Payload...))
		require.Equal(t, hex.EncodeToString(legacy[:]), EventDigest(e))
		require.Equal(t, EventDigest(e), EventDigest(decoded))
	}
	require.NotEqual(t, EventDigest(Event{Kind: "billing", Payload: json.RawMessage(`{"id":9007199254740993}`)}), EventDigest(Event{Kind: "billing", Payload: json.RawMessage(`{"id":9007199254740992}`)}))
}

func TestReceiptProofRejectsMissingDuplicateAndWrongPayload(t *testing.T) {
	digest := EventDigest(Event{Kind: "billing", Payload: json.RawMessage(`{"a":1}`)})
	expected := map[int64]string{1: digest}
	require.True(t, ValidReceiptDigests([]int64{1}, expected))
	require.False(t, ValidReceiptDigests([]int64{1, 1}, map[int64]string{1: digest, 2: digest}))
	require.False(t, ValidReceiptDigests([]int64{1}, map[int64]string{1: strings.ToUpper(digest)}))
	require.NoError(t, validateReceiptProofs(expected, expected, []int64{1}, nil))
	require.Error(t, validateReceiptProofs(expected, nil, []int64{1}, nil))
	require.Error(t, validateReceiptProofs(expected, map[int64]string{1: strings.Repeat("0", 64)}, nil, []Rejection{{ID: 1, Reason: "conflict"}}))
	require.True(t, ValidReplicaID(""))
	require.False(t, ValidReplicaID("../../other"))
	require.False(t, ValidReplicaID("00000000-0000-0000-0000-000000000000"))
}
