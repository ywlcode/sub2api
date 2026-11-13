package site

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportedRequestIDPreservesBillingIdentity(t *testing.T) {
	t.Parallel()
	// A fixed pre-existing format is required for replay deduplication and for
	// displaying the badge on usage imported before the badge was introduced.
	const expected = "site:e77e3425d25e1659d2fc1e99ab0ceb40b3af72303735203674799e72"
	require.Equal(t, expected, ImportedRequestID("office", "client:request-one"))
	require.Equal(t, expected, ImportedRequestID("office", "local:client:request-one"))
	require.NotEqual(t, expected, ImportedRequestID("office-two", "client:request-one"))
	require.True(t, IsImportedRequestID(expected))
}

func TestIsImportedRequestIDRejectsOrdinaryAndClientSuppliedIDs(t *testing.T) {
	t.Parallel()
	id := ImportedRequestID("office", "req-one")
	for _, requestID := range []string{
		"", "req-one", "local:req-one", "client:req-one", "site:",
		"client:" + id, "local:" + id, "web_search:" + id,
		id[:len(id)-1], id + "0", " " + id, id + " ",
		"site:" + strings.Repeat("g", 56), strings.ToUpper(id),
	} {
		t.Run(requestID, func(t *testing.T) {
			require.False(t, IsImportedRequestID(requestID))
		})
	}
}
