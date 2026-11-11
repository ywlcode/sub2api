package service

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/jsonview"
	"github.com/tidwall/gjson"
)

func volcengineHasPlaintextReasoning(item gjson.Result) bool {
	var value map[string]any
	if decodeOpenAIJSONUseNumber([]byte(item.Raw), &value) != nil {
		return false
	}
	return len(deepSeekReasoningParts(value["content"], "reasoning_text")) > 0 ||
		(value["encrypted_content"] == nil || value["encrypted_content"] == "") &&
			len(deepSeekReasoningParts(value["summary"], "summary_text")) > 0
}

// Ark compatibility is stateless: clients replay the complete plaintext history.
// Validate before any generic normalizer can drop state references or ciphertext.
func validateVolcengineResponsesRequest(account *Account, body []byte) error {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() {
		return nil
	}
	request := jsonview.Parse(body)
	if !gjson.ValidBytes(body) || !request.IsObject() {
		return fmt.Errorf("volcengine requires a valid Responses request object")
	}
	fields := normalizationObjectFields(request)
	if err := validateVolcengineStatelessHistory(fields); err != nil {
		return err
	}
	if fields["messages"].Exists() {
		return fmt.Errorf("Responses requires input; send Chat messages to /v1/chat/completions")
	}
	reasoning := normalizationObjectFields(fields["reasoning"])
	thinking := normalizationObjectFields(fields["thinking"])
	effort := volcengineEffort(reasoning["effort"].String())
	if effort == "" {
		effort = volcengineEffort(fields["reasoning_effort"].String())
	}
	if reasoning["summary"].String() == "none" && effort != "none" && volcengineEffort(thinking["type"].String()) != "none" {
		return fmt.Errorf("volcengine compatibility requires plaintext reasoning for replay; reasoning.summary:none is not supported while thinking is enabled (the client may hide thinking locally)")
	}
	input := fields["input"]
	if input.Type != gjson.String && !input.IsArray() {
		return fmt.Errorf("volcengine Responses input must be a string or a complete history array")
	}
	pending, seen := make(map[string]bool), make(map[string]bool)
	for _, item := range input.Array() {
		if item.Get("type").String() == "" && item.Get("id").String() != "" && !item.Get("role").Exists() && !item.Get("content").Exists() {
			return fmt.Errorf("volcengine compatibility does not support ID-only item references; resend the complete plaintext history")
		}
		switch item.Get("type").String() {
		case "reasoning":
			if !volcengineHasPlaintextReasoning(item) {
				return fmt.Errorf("reasoning item %q has no recoverable plaintext; resend the complete reasoning history (ID-only or encrypted history cannot be restored)", item.Get("id").String())
			}
		case "item_reference", "compaction", "compaction_trigger":
			return fmt.Errorf("volcengine compatibility does not support %s; resend the complete plaintext history", item.Get("type").String())
		case "function_call", "custom_tool_call":
			id := item.Get("call_id").String()
			if id == "" || seen[id] {
				return fmt.Errorf("tool history contains a missing or duplicate call_id")
			}
			seen[id], pending[id] = true, true
		case "function_call_output", "custom_tool_call_output":
			id := item.Get("call_id").String()
			if !pending[id] {
				return fmt.Errorf("tool output %q has no matching pending call; resend the complete tool history", id)
			}
			delete(pending, id)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("tool history is missing results; resend the complete call and result pairs")
	}
	return nil
}

func validateVolcengineStatelessHistory(fields map[string]gjson.Result) error {
	if fields["store"].Bool() || fields["previous_response_id"].String() != "" || fields["conversation"].Exists() && fields["conversation"].Type != gjson.Null {
		return fmt.Errorf("volcengine compatibility requires stateless full history; store:true, previous_response_id and conversation are not supported")
	}
	return nil
}

// Responses Lite declares tools in input[].additional_tools. Ark must receive
// them at the top level; preserve raw definitions, including provider extensions.
func promoteVolcengineAdditionalTools(account *Account, body []byte) ([]byte, error) {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() || !gjson.GetBytes(body, "input.#(type==additional_tools)").Exists() {
		return body, nil
	}
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return nil, err
	}
	tools, ok := request["tools"].([]any)
	if value, exists := request["tools"]; exists && value != nil && !ok {
		return nil, fmt.Errorf("responses tools must be an array")
	}
	items, _ := request["input"].([]any)
	input := make([]any, 0, len(items))
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok || item["type"] != "additional_tools" {
			input = append(input, value)
			continue
		}
		additional, ok := item["tools"].([]any)
		if !ok {
			return nil, fmt.Errorf("responses additional_tools.tools must be an array")
		}
		tools = append(tools, additional...)
	}
	request["input"], request["tools"] = input, tools
	return marshalOpenAIUpstreamJSON(request)
}

func adaptVolcengineResponsesClientTools(body []byte) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return nil, apicompat.ResponsesClientToolMapping{}, err
	}
	mapping, changed, err := apicompat.AdaptResponsesToolSearch(request)
	if err != nil || !changed {
		return body, mapping, err
	}
	adapted, err := marshalOpenAIUpstreamJSON(request)
	return adapted, mapping, err
}

func volcengineResponsesTerminalFailure(eventType string, payload []byte) bool {
	switch eventType {
	case "error", "response.failed", "response.cancelled", "response.canceled":
		return true
	}
	switch gjson.GetBytes(payload, "response.status").String() {
	case "failed", "cancelled", "canceled":
		return true
	}
	return false
}
