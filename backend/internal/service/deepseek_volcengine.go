package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// DeepSeekVolcengineCompatibilityCredential is explicit because DeepSeek accounts
// may target Ark directly or through another relay. Never infer it from a model alias.
const DeepSeekVolcengineCompatibilityCredential = "deepseek_volcengine_compatibility"

func (a *Account) IsDeepSeekVolcengineCompatibilityEnabled() bool {
	if a == nil || a.Platform != PlatformDeepseek || a.Type != AccountTypeAPIKey {
		return false
	}
	enabled, _ := a.Credentials[DeepSeekVolcengineCompatibilityCredential].(bool)
	return enabled
}

// normalizeVolcengineRequestBody only translates known compatibility aliases.
// Unknown fields, tool arguments, literal tags and user text are not scrubbed.
func normalizeVolcengineRequestBody(account *Account, body []byte, protocol string) ([]byte, error) {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() || len(body) == 0 {
		return body, nil
	}
	if !volcengineRequestNeedsNormalization(body, protocol) {
		return body, nil
	}
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return nil, fmt.Errorf("normalize Volcengine request: %w", err)
	}
	if request == nil {
		return body, nil
	}
	normalizeVolcengineReasoning(request, protocol)
	normalizeDeepSeekReasoningInput(request["input"], true)
	if items, ok := request["input"].([]any); ok {
		for _, value := range items {
			if item, ok := value.(map[string]any); ok && item["type"] == "reasoning" && len(deepSeekReasoningParts(item["summary"], "summary_text")) > 0 {
				// This account replays plaintext. Do not attach a foreign
				// provider's ciphertext/signature to the converted Ark item.
				delete(item, "encrypted_content")
			}
		}
	}
	for _, field := range []string{"input", "messages"} {
		items, _ := request[field].([]any)
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			delete(item, "internal_chat_message_metadata_passthrough")
			if field == "input" {
				if volcengineUntypedMessage(item) {
					item["type"] = "message"
				}
				parts, _ := item["content"].([]any)
				for _, value := range parts {
					if part, ok := value.(map[string]any); ok && part["type"] == "output_text" {
						delete(part, "logprobs")
					}
				}
			}
		}
	}
	return marshalOpenAIUpstreamJSON(request)
}

func volcengineUntypedMessage(item map[string]any) bool {
	if typ, exists := item["type"]; exists && typ != nil && typ != "" {
		return false
	}
	// Do not relabel tool/reasoning items that happen to carry role/content.
	for _, key := range []string{"call_id", "tool_call_id", "tool_calls", "function_call", "arguments", "output", "encrypted_content"} {
		if _, exists := item[key]; exists {
			return false
		}
	}
	switch item["role"] {
	case "system", "developer", "user", "assistant":
	default:
		return false
	}
	switch item["content"].(type) {
	case string, []any:
		return true
	}
	return false
}

func volcengineEffort(value any) string {
	effort, _ := value.(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "off", "disabled":
		return "none"
	default:
		return effort
	}
}

func normalizeVolcengineReasoning(request map[string]any, protocol string) {
	thinking, _ := request["thinking"].(map[string]any)
	mode := volcengineEffort(thinking["type"])
	if protocol == APIProtocolAnthropic {
		output, _ := request["output_config"].(map[string]any)
		effort := volcengineEffort(output["effort"])
		if mode == "none" || effort == "none" {
			request["thinking"] = map[string]any{"type": "disabled"}
			delete(output, "effort")
			if len(output) == 0 {
				delete(request, "output_config")
			}
		} else if effort != "" {
			output["effort"] = effort
			// Ark only honors output_config.effort in adaptive mode. Claude's
			// manual token budget must not cap DeepSeek's requested effort.
			request["thinking"] = map[string]any{"type": "adaptive"}
		}
		return
	}
	// Native nested effort wins over the legacy flat alias, as in the bridges.
	reasoning, _ := request["reasoning"].(map[string]any)
	effort := volcengineEffort(reasoning["effort"])
	if effort == "" {
		effort = volcengineEffort(request["reasoning_effort"])
	}
	if mode == "none" {
		effort = "none"
	}
	if effort == "" {
		return
	}
	// Use a single control so Ark never sees disabled + high, or enabled + none.
	if mode == "none" || mode == "enabled" {
		delete(request, "thinking")
	}
	if reasoning == nil {
		reasoning = make(map[string]any)
	}
	reasoning["effort"] = effort
	request["reasoning"] = reasoning
	delete(request, "reasoning_effort")
}

// Apply the final Responses adaptation before gzip, including admin probes.
// Account scope also works when the next hop is another relay.
func accountVolcengineHTTP(upstream HTTPUpstream, account *Account) HTTPUpstream {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() {
		return upstream
	}
	return &volcengineHTTP{HTTPUpstream: upstream, account: account}
}

type volcengineHTTP struct {
	HTTPUpstream
	account *Account
}

func (u *volcengineHTTP) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.Do(prepared, proxy, accountID, concurrency)
	})
}

func (u *volcengineHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.DoWithTLS(prepared, proxy, accountID, concurrency, profile)
	})
}

func (u *volcengineHTTP) send(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || req.Body == nil {
		return send(req)
	}
	defer func() { _ = req.Body.Close() }()
	var protocol string
	switch path := strings.TrimRight(req.URL.Path, "/"); {
	case strings.HasSuffix(path, "/responses"):
		protocol = APIProtocolResponses
	case strings.HasSuffix(path, "/chat/completions"):
		return nil, fmt.Errorf("Volcengine compatibility only supports Responses upstream; Chat Completions is disabled")
	case strings.HasSuffix(path, "/messages"):
		return nil, fmt.Errorf("Volcengine compatibility only supports Responses upstream; Anthropic Messages must be converted first")
	case strings.HasSuffix(path, "/responses/compact"):
		return nil, fmt.Errorf("Volcengine compatibility does not support /responses/compact")
	default:
		return send(req)
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("normalize Volcengine request: JSON request body must be replayable")
	}
	if err := context.Cause(req.Context()); err != nil {
		return nil, err
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("normalize Volcengine request: GetBody returned no body")
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	if err := validateVolcengineResponsesRequest(u.account, body); err != nil {
		return nil, err
	}
	body, err = normalizeVolcengineRequestBody(u.account, body, protocol)
	if err != nil {
		return nil, err
	}
	prepared := req.Clone(req.Context())
	prepared.Body = io.NopCloser(bytes.NewReader(body))
	prepared.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	prepared.ContentLength = int64(len(body))
	prepared.TransferEncoding = nil
	prepared.Header.Del("Content-Length")
	if isVolcengineTextCompaction(body) {
		return forwardVolcengineTextCompaction(prepared, body, func(attempt *http.Request) (*http.Response, error) {
			resp, err := send(attempt)
			if err == nil && resp != nil && resp.Body != nil && isEventStreamResponse(resp.Header) {
				resp.Body = newVolcengineSSEBodyWithLimit(resp.Body, volcengineCompactionMaxEventBytes)
			}
			return resp, err
		})
	}
	resp, err := send(prepared)
	if err == nil && resp != nil && resp.Body != nil && isEventStreamResponse(resp.Header) {
		resp.Body = newVolcengineSSEBody(resp.Body)
	}
	return resp, err
}
