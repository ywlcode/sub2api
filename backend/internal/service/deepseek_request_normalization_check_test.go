package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDeepSeekNormalizedBodyReusesImmutableBytes(t *testing.T) {
	account := volcengineTestAccount()
	for _, content := range []string{strings.Repeat("large plain text ", 10000), strings.Repeat(`escaped\ntext\" `, 10000)} {
		body := []byte(`{"store":false,"seed":9007199254740993,"reasoning":{"effort":"high"},"input":[` +
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"real thought"}]},` +
			`{"type":"message","role":"user","content":"` + content + `"},` +
			`{"type":"function_call_output","call_id":"c","output":"{\"content\":{\"text\":\"ordinary tool data\"}}"}]}`)
		original := bytes.Clone(body)
		for range 3 { // Ingress, builder, final transport, including retries.
			got, err := normalizeVolcengineRequestBody(account, body, APIProtocolResponses)
			require.NoError(t, err)
			require.Same(t, &body[0], &got[0], "already-normalized Ark body should not be rebuilt")
			got = normalizeDeepSeekResponsesRequestBody(account, got)
			require.Same(t, &body[0], &got[0], "CN adaptation must also skip an unchanged body")
		}
		require.Equal(t, original, body, "borrowed body must remain safe for another account or retry")
	}
}

func TestVolcengineNormalizationCheckPreservesDecoderSemantics(t *testing.T) {
	for _, body := range []string{
		`{"reasoning_effort":"off","reasoning":{"summary":1e1000},"seed":9007199254740993}`,
		`{"reasoning_effort":"off","reasoning":{"summary":9007199254740993}}`,
		`{"input":[{"type":"message","type":"","role":"user","content":"hi"}]}`,
		`{"input":[{"type":"message","role":"assistant","content":[{"type":"input_text","type":"output_text","text":"x","logprobs":[]}]}]}`,
		`{"input":[],"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"plan"}]}]}`,
		`{"input":[{"role":"user","content":"hi","internal_chat_message_metadata_pass\u0074hrough":null}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			require.True(t, volcengineRequestNeedsNormalization([]byte(body), APIProtocolResponses))
			got, err := normalizeVolcengineRequestBody(volcengineTestAccount(), []byte(body), APIProtocolResponses)
			require.NoError(t, err)
			require.False(t, volcengineRequestNeedsNormalization(got, APIProtocolResponses))
			if value := gjson.GetBytes([]byte(body), "reasoning.summary"); value.Exists() {
				require.Equal(t, value.Raw, gjson.GetBytes(got, "reasoning.summary").Raw)
				require.Equal(t, "none", gjson.GetBytes(got, "reasoning.effort").Str)
			}
		})
	}
	for _, invalid := range []string{`{`, `{} {}`, `[]`, `{"input":NaN}`} {
		_, err := normalizeVolcengineRequestBody(volcengineTestAccount(), []byte(invalid), APIProtocolResponses)
		require.Error(t, err)
	}
}

func TestDeepSeekNormalizationCheckStillLiftsEncodedToolMedia(t *testing.T) {
	for _, output := range []string{
		`"data:image/png;base64,YQ=="`,
		`"[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,YQ==\"}]"`,
		`{"content":[{"type":"input_image","image_url":{"url":"data:image/png;base64,YQ=="}}]}`,
		`[{"type":"text","type":"input_image","image_url":"data:image/png;base64,YQ=="}]`,
		`[{"type":"input_\u0069mage","image_url":"data:image/png;base64,YQ=="}]`,
	} {
		body := []byte(`{"input":[{"type":"function_call_output","call_id":"c","output":` + output + `}]}`)
		require.True(t, deepSeekInputNeedsNormalization(body, true, true, true, "image_url"))
		got := normalizeDeepSeekResponsesRequestBody(volcengineTestAccount(), body)
		require.True(t, gjson.GetBytes(got, "input.1.content.#(type==input_image)").Exists(), string(got))
		require.False(t, deepSeekInputNeedsNormalization(got, true, true, true, "image_url"), "lifted media must not cause another full decode")
	}
}
