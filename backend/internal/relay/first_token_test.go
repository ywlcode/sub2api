package relay

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestSemanticDelta(t *testing.T) {
	for _, test := range []struct {
		name, data string
		want       bool
	}{
		{"metadata", `{"type":"response.created"}`, false},
		{"empty delta", `{"type":"response.output_text.delta","delta":""}`, false},
		{"text", `{"type":"response.output_text.delta","delta":"hello"}`, true},
		{"reasoning", `{"type":"response.reasoning_text.delta","delta":"think"}`, true},
		{"role", `{"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"chat text", `{"choices":[{"delta":{"content":"hello"}}]}`, true},
		{"chat reasoning", `{"choices":[{"delta":{"reasoning_content":"think"}}]}`, true},
		{"tool arguments", `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}`, true},
		{"done", "[DONE]", false},
		{"invalid", "not JSON", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if semanticDelta([]byte(test.data), "") != test.want {
				t.Fatal("incorrect first-token classification")
			}
		})
	}
}

func TestDeltaObserverHandlesChunkedEventsAndKeepsBytes(t *testing.T) {
	stream := ": heartbeat\r\n\r\ndata: {\"type\":\"response.created\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\r\ndata: \"delta\":\"hello\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"again\"}\r\n\r\n"
	calls := 0
	at := time.Now()
	observer := deltaObserver{first: func(got time.Time) {
		calls++
		if !got.Equal(at) {
			t.Fatal("arrival timestamp changed")
		}
	}}
	for i := range len(stream) {
		observer.feed([]byte(stream[i:i+1]), at)
	}
	if calls != 1 || observer.line != nil || observer.data != nil {
		t.Fatal("observer retained events or counted tokens twice")
	}
	calls = 0
	body := firstTokenBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), observer: deltaObserver{first: func(time.Time) { calls++ }}}
	got, err := io.ReadAll(&body)
	if err != nil || string(got) != stream || calls != 1 {
		t.Fatalf("observer modified stream: %v, calls=%d", err, calls)
	}
}

func TestDeltaObserverDoesNotMistakeLaterTokenAfterOversizedEvent(t *testing.T) {
	stream := "data: " + strings.Repeat("x", maxObservedEventBytes+50) + "\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\r\n\r\n"
	calls := 0
	observer := deltaObserver{first: func(time.Time) { calls++ }}
	for i := 0; i < len(stream); i += 17 {
		observer.feed([]byte(stream[i:min(len(stream), i+17)]), time.Now())
		if len(observer.line)+len(observer.data) > maxObservedEventBytes {
			t.Fatal("SSE observation exceeded memory cap")
		}
	}
	if calls != 0 || !observer.done {
		t.Fatalf("later token incorrectly reported as first: %d", calls)
	}
}
