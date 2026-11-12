package site

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// KeyDigest lets the replica authenticate clients without holding reusable
// customer API keys. Each paired replica has a different lookup namespace.
func KeyDigest(token, key string) string {
	h := hmac.New(sha256.New, []byte(token))
	_, _ = h.Write([]byte("sub2api-site-key-v1\x00" + key))
	return hex.EncodeToString(h.Sum(nil))
}
