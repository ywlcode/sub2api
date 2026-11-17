package service

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// When recovery is refused/revoked, render the original native SSE terminal
// without reclassifying it as a reason to rotate accounts. This grants no retry.
type openAI200RejectedTerminalContextKey struct{}

func (s *SettingService) IsOpenAI200OmitEncryptedHistoryEnabled(ctx context.Context) bool {
	return s.isOpenAIHistoryRecoveryEnabled(ctx, SettingKeyOpenAI200OmitEncryptedHistoryEnabled)
}

// Only native HTTP 200 SSE rejections before delivery reach this entry point.
// It grants no additional budget beyond the shared 400/200 lossy recovery.
func (s *OpenAIGatewayService) prepareOpenAI200HistoryRecovery(ctx context.Context, c *gin.Context, account *Account, body []byte, attempts int) (context.Context, []byte, bool) {
	return s.prepareOpenAIRejectedHistoryRecovery(ctx, c, account, body, attempts, SettingKeyOpenAI200OmitEncryptedHistoryEnabled, openAI200HistoryDiagnosticKey)
}

// CC Switch v4.0.32 also recognizes this complete upstream diagnostic in a
// structured error field. Never match arbitrary response text, metadata, or a
// generic mention of encryption. The existing precise/HTTP classifier stays
// unchanged; this additional recognition is confined to the 200 Beta path.
// https://github.com/farion1231/cc-switch/blob/v4.0.32/src-tauri/src/proxy/opaque_state_rectifier.rs
func isOpenAI200EncryptedHistoryRejection(payload []byte, eventType string) bool {
	if len(payload) > 65536 || !openAIEncryptedJSONUnambiguous(payload) {
		return false
	}
	switch eventType {
	case "response.failed":
		if !gjson.GetBytes(payload, "response.error").IsObject() {
			return false
		}
	case "error":
	default:
		return false
	}
	code, message := openAIEncryptedRejectionFields(payload)
	if isOpenAIInvalidEncryptedContentError(code, message) {
		return true
	}
	// The fixed function-output rejection is also emitted without a code.
	root := gjson.ParseBytes(payload)
	if root.Get("response.error").IsObject() && root.Get("error").Exists() {
		return false
	}
	if root.Get("response.error").IsObject() || root.Get("error").IsObject() {
		if root.Get("code").Exists() || root.Get("message").Exists() {
			return false
		}
		if root.Get("response.error").IsObject() {
			root = root.Get("response.error")
		} else {
			root = root.Get("error")
		}
	}
	text := root.Get("message")
	return text.Type == gjson.String && strings.Contains(text.String(), "Encrypted function output content could not be decrypted or decoded.")
}
