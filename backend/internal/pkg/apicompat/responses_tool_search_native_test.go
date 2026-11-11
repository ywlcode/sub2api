package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAdaptResponsesToolSearchPreservesNativeToolsAndHistory(t *testing.T) {
	body := `{
		"tools":[{"type":"tool_search"},{"type":"custom","name":"exec","format":{"type":"text"}},
			{"type":"namespace","name":"team","tools":[{"type":"function","name":"tool_search","parameters":{"type":"object"}}]}],
		"tool_choice":{"type":"custom","name":"exec"},
		"input":[
			{"type":"custom_tool_call","id":"ctc_original","call_id":"custom_pair","name":"exec","input":"echo hello"},
			{"type":"custom_tool_call_output","id":"ctco_original","call_id":"custom_pair","output":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","id":"fc_namespace","call_id":"namespace_pair","namespace":"team","name":"tool_search","arguments":"{}"},
			{"type":"tool_search_call","id":"tsc_lookup","call_id":"search_pair","arguments":{"query":"lookup"}},
			{"type":"tool_search_output","call_id":"search_pair","tools":[{"type":"namespace","name":"discovered","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}
		]}`
	var request map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	mapping, changed, err := AdaptResponsesToolSearch(request)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, mapping.ToolSearch)
	require.Empty(t, mapping.CustomTools)
	require.Empty(t, mapping.NamespaceTools)
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	for _, path := range []string{"tools.1", "tools.2", "tool_choice", "input.0", "input.1", "input.2"} {
		require.JSONEq(t, gjson.Get(body, path).Raw, gjson.GetBytes(encoded, path).Raw, path)
	}
	require.Equal(t, "function", gjson.GetBytes(encoded, "tools.0.type").String())
	require.Equal(t, "discovered", gjson.GetBytes(encoded, "tools.3.name").String())
	require.Equal(t, "namespace", gjson.GetBytes(encoded, "tools.3.type").String())
	require.Equal(t, "fc_lookup", gjson.GetBytes(encoded, "input.3.id").String())
	require.Equal(t, "search_pair", gjson.GetBytes(encoded, "input.4.call_id").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(encoded, "input.4.type").String())

	// A namespace can contain a function called tool_search. Only the top-level
	// proxy call may be raised; native item IDs and opaque fields must survive.
	payload := []byte(`{"type":"response.completed","sequence_number":10,"response":{"output":[
		{"type":"custom_tool_call","id":"ctc_own","call_id":"c","name":"exec","input":"echo hi","opaque":9007199254740993},
		{"type":"function_call","id":"fc_ns","call_id":"n","namespace":"team","name":"tool_search","arguments":"{}"},
		{"type":"function_call","id":"fc_find","call_id":"s","name":"tool_search","arguments":"{\"query\":\"lookup\"}"}]}}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	for _, path := range []string{"response.output.0", "response.output.1"} {
		require.JSONEq(t, gjson.GetBytes(payload, path).Raw, gjson.GetBytes(restored, path).Raw)
	}
	require.Equal(t, "9007199254740993", gjson.GetBytes(restored, "response.output.0.opaque").Raw)
	require.Equal(t, "tool_search_call", gjson.GetBytes(restored, "response.output.2.type").String())
	require.Equal(t, "tsc_find", gjson.GetBytes(restored, "response.output.2.id").String())
	require.Equal(t, "s", gjson.GetBytes(restored, "response.output.2.call_id").String())

	restorer := NewResponsesClientToolStreamRestorer(mapping)
	for _, item := range gjson.GetBytes(payload, "response.output").Array()[:2] {
		event := []byte(`{"type":"response.output_item.added","sequence_number":0,"item":` + item.Raw + `}`)
		frames, _, err := restorer.RestoreEvent(event)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		require.JSONEq(t, item.Raw, gjson.GetBytes(frames[0], "item").Raw)
	}
	frames, _, err := restorer.RestoreEvent(payload)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.Equal(t, "9007199254740993", gjson.GetBytes(frames[0], "response.output.0.opaque").Raw)
	require.JSONEq(t, gjson.GetBytes(restored, "response.output").Raw, gjson.GetBytes(frames[0], "response.output").Raw)
	_, _, err = RestoreResponsesClientToolPayload([]byte(`{} {}`), mapping)
	require.Error(t, err, "native tool restoration must still reject trailing JSON")
}

func TestAdaptResponsesToolSearchDoesNotRewriteCustomResultContents(t *testing.T) {
	var request map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"tools":[{"type":"tool_search"}],"input":[{"type":"custom_tool_call_output","call_id":"original","output":[{"type":"tool_search_call","id":"tsc_literal","call_id":"literal","arguments":{"query":"business data"}}]}]}`), &request))
	original, err := json.Marshal(request["input"])
	require.NoError(t, err)
	_, _, err = AdaptResponsesToolSearch(request)
	require.NoError(t, err)
	after, err := json.Marshal(request["input"])
	require.NoError(t, err)
	require.JSONEq(t, string(original), string(after))
}

func TestAdaptResponsesToolSearchHistoryWithoutDeclaration(t *testing.T) {
	for _, declaration := range []string{"", `"tools":null,`, `"tools":[],`, `"tools":[{"type":"function","name":"tool_search","parameters":{"type":"object"}}],`} {
		t.Run(declaration, func(t *testing.T) {
			body := []byte(`{` + declaration + `"input":[{"type":"tool_search_call","id":"tsc_history","call_id":"search_pair","arguments":{"query":"lookup"}},{"type":"tool_search_output","call_id":"search_pair","tools":[{"type":"function","name":"old_discovery","parameters":{"type":"object"}}]}]}`)
			var request map[string]any
			require.NoError(t, json.Unmarshal(body, &request))
			mapping, changed, err := AdaptResponsesToolSearch(request)
			require.NoError(t, err)
			require.True(t, changed)
			require.False(t, mapping.ToolSearch, "history alone must not change current response restoration")
			encoded, err := json.Marshal(request)
			require.NoError(t, err)
			beforeTools, afterTools := gjson.GetBytes(body, "tools"), gjson.GetBytes(encoded, "tools")
			require.Equal(t, beforeTools.Exists(), afterTools.Exists())
			if beforeTools.Exists() {
				require.JSONEq(t, beforeTools.Raw, afterTools.Raw)
			}
			require.Equal(t, "function_call", gjson.GetBytes(encoded, "input.0.type").String())
			require.Equal(t, "fc_history", gjson.GetBytes(encoded, "input.0.id").String())
			require.Equal(t, "function_call_output", gjson.GetBytes(encoded, "input.1.type").String())
			require.Equal(t, "search_pair", gjson.GetBytes(encoded, "input.1.call_id").String())
			_, changed, err = AdaptResponsesToolSearch(request)
			require.NoError(t, err)
			require.False(t, changed, "normalization must be idempotent")

			// The general adapter used by other providers retains its policy.
			require.NoError(t, json.Unmarshal(body, &request))
			_, changed, err = AdaptResponsesClientTools(request)
			require.NoError(t, err)
			require.False(t, changed)
		})
	}
}

func TestAdaptResponsesToolSearchPreservesHostedHistory(t *testing.T) {
	for _, declaration := range []string{"", `"tools":[],`, `"tools":[{"type":"tool_search","execution":"client"}],`} {
		t.Run(declaration, func(t *testing.T) {
			body := []byte(`{` + declaration + `"input":[{"type":"tool_search_call","execution":"server","call_id":null,"arguments":{"paths":["lookup"]},"status":"completed"},{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`)
			var request map[string]any
			require.NoError(t, json.Unmarshal(body, &request))
			_, _, err := AdaptResponsesToolSearch(request)
			require.NoError(t, err)
			encoded, err := json.Marshal(request)
			require.NoError(t, err)
			require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(encoded, "input").Raw)
		})
	}
}
