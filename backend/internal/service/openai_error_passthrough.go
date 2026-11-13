package service

import (
	"errors"
	"net/http"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// errOpenAIErrorPassthroughForwarded prevents compatibility paths from billing
// an error as a successful response or writing a second response. Cyber risk
// accounting remains driven exclusively by the original CyberPolicyMark.
var errOpenAIErrorPassthroughForwarded = errors.New("openai error passthrough forwarded to client")

// Client rule matching is independent of risk classification. An already blocked
// session is a semantic 403, but only exact cyber_policy records a new risk hit.
// A real HTTP 403 must be eligible regardless of its error code or body format.
func isOpenAIForbiddenPassthrough(payload []byte, upstreamStatus int) bool {
	if hit, _, _ := detectOpenAICyberPolicy(payload); hit || isOpenAIUpstreamSessionBlocked(payload) {
		return true
	}
	if upstreamStatus >= http.StatusBadRequest {
		return upstreamStatus == http.StatusForbidden
	}
	// For HTTP 200 / SSE / WS, require a structured error. Never interpret model
	// output, echoed request data, or an unrelated numeric status as a rejection.
	if !gjson.GetBytes(payload, "error").IsObject() && !gjson.GetBytes(payload, "response.error").IsObject() {
		return false
	}
	return openAIStreamFailedEventSemanticStatus(payload, extractOpenAISSEErrorMessage(payload)) == http.StatusForbidden
}

// rewriteOpenAIForbiddenErrorForClient returns a client-only copy. Cyber errors
// use a fixed local envelope; other errors retain rule behavior. Pass the actual HTTP status, or 0 for an
// event. Account health, retries and audit evidence must use the original body.
// The returned status can only change HTTP headers before they are committed.
func rewriteOpenAIForbiddenErrorForClient(c *gin.Context, account *Account, payload []byte, upstreamStatus int) ([]byte, int, bool) {
	if !isOpenAIForbiddenPassthrough(payload, upstreamStatus) {
		return payload, 0, false
	}
	if isCyberPolicyRejection(payload) {
		return cyberPolicyClientPayload(payload), http.StatusForbidden, true
	}
	platform := PlatformOpenAI
	if account != nil {
		platform = account.Platform
	}
	status, errType, message, matched := applyErrorPassthroughRule(c, platform, http.StatusForbidden,
		openAIStreamFailedEventPassthroughBody(payload, ""), http.StatusForbidden, "upstream_error", "")
	if !matched {
		return payload, 0, false
	}
	clientPayload := payload
	rewrittenError := false
	for _, path := range []string{"error", "response.error"} {
		if !gjson.GetBytes(payload, path).IsObject() {
			continue
		}
		var err error
		clientPayload, err = sjson.SetBytes(clientPayload, path+".message", message)
		if err != nil {
			return payload, 0, false
		}
		clientPayload, err = sjson.SetBytes(clientPayload, path+".type", errType)
		if err != nil {
			return payload, 0, false
		}
		rewrittenError = true
	}
	if !rewrittenError {
		// HTML/plain-text 403 responses can match status or keyword rules too.
		clientPayload, _ = marshalOpenAIUpstreamJSON(gin.H{"error": gin.H{"type": errType, "message": message}})
	}
	for _, path := range openAIStreamErrorStatusPaths {
		if gjson.GetBytes(payload, path).Type == gjson.Number {
			if rewritten, err := sjson.SetBytes(clientPayload, path, status); err == nil {
				clientPayload = rewritten
			}
		}
	}
	return clientPayload, status, true
}

func writeOpenAIErrorPassthroughClientError(c *gin.Context, status int, payload []byte) {
	body := openAIStreamFailedEventPassthroughBody(payload, "")
	MarkResponseCommitted(c)
	if !writeOpenAICompactSSEBridge(c, status, body) {
		c.Data(status, "application/json; charset=utf-8", body)
	}
}

// A failed upstream handshake has no event stream. Before closing an already
// upgraded downstream WS, deliver the matched 403 as a structured error event.
func forwardOpenAIForbiddenWSDialError(c *gin.Context, account *Account, upstreamErr error, writeClient func([]byte) error) (error, bool) {
	var dialErr *openAIWSDialError
	if !errors.As(upstreamErr, &dialErr) || dialErr == nil {
		return upstreamErr, false
	}
	markOpenAICyberPolicyEvent(c, dialErr.ResponseBody, dialErr.StatusCode, nil)
	clientBody, status, matched := rewriteOpenAIForbiddenErrorForClient(c, account, dialErr.ResponseBody, dialErr.StatusCode)
	if !matched {
		return upstreamErr, false
	}
	setOpsUpstreamError(c, dialErr.StatusCode, extractUpstreamErrorMessage(dialErr.ResponseBody), "")
	clientBody = openAIStreamFailedEventPassthroughBody(clientBody, "")
	message := extractUpstreamErrorMessage(clientBody)
	event := buildOpenAIWSHTTPBridgeErrorEvent(status, message)
	if code := gjson.GetBytes(clientBody, "error.code").String(); code != "" {
		event, _ = sjson.SetBytes(event, "error.code", code)
	}
	event, _ = sjson.SetBytes(event, "error.type", gjson.GetBytes(clientBody, "error.type").String())
	if err := writeClient(event); err != nil {
		return err, true
	}
	markOpenAIWSClientVisibleFailure(c, "error", event)
	return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, message, upstreamErr), true
}
