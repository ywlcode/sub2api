package site

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	importedRequestIDPrefix = "site:"
	importedRequestIDBytes  = 28
)

// ImportedRequestID namespaces a secondary's settled request on the primary.
// Keep this format stable: existing billing deduplication keys use these IDs.
func ImportedRequestID(node, requestID string) string {
	requestID = strings.TrimPrefix(requestID, "local:")
	sum := sha256.Sum256([]byte(node + "\x00" + requestID))
	return importedRequestIDPrefix + hex.EncodeToString(sum[:importedRequestIDBytes])
}

// IsImportedRequestID identifies usage imported from a secondary. Requests
// merely proxied through a secondary are billed by the primary with its normal
// request IDs. Client-provided IDs are prefixed with "client:" before billing.
func IsImportedRequestID(requestID string) bool {
	if len(requestID) != len(importedRequestIDPrefix)+2*importedRequestIDBytes || !strings.HasPrefix(requestID, importedRequestIDPrefix) {
		return false
	}
	for _, c := range requestID[len(importedRequestIDPrefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
