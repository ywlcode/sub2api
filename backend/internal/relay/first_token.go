package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// Observe a bounded SSE event until the first semantic delta. Read always
// returns the original bytes: SSE framing, flushing and cancellation are intact.
const maxObservedEventBytes = 64 << 10

type firstTokenBody struct {
	io.ReadCloser
	observer deltaObserver
}

func (r *firstTokenBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.observer.feed(p[:n], time.Now())
	}
	return n, err
}

type deltaObserver struct {
	line      []byte
	data      []byte
	eventType string
	done      bool
	first     func(time.Time)
}

func (o *deltaObserver) feed(p []byte, received time.Time) {
	if o.done {
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		piece := p
		if i >= 0 {
			piece = p[:i]
		}
		if len(o.line)+len(o.data)+len(piece) > maxObservedEventBytes {
			// An unobserved event might contain the first token. Do not report
			// a later token as the first; just leave this request unmeasured.
			o.done = true
			o.line, o.data, o.eventType = nil, nil, ""
			return
		}
		o.line = append(o.line, piece...)
		if i < 0 {
			return
		}
		p = p[i+1:]
		line := bytes.TrimSuffix(o.line, []byte{'\r'})
		if len(line) == 0 {
			if semanticDelta(o.data, o.eventType) {
				o.done = true
				o.line = nil
				o.data = nil
				o.first(received)
				return
			}
			o.data = o.data[:0]
			o.eventType = ""
		} else if bytes.HasPrefix(line, []byte("event:")) {
			name := bytes.TrimSpace(line[len("event:"):])
			if len(name) <= 128 {
				o.eventType = string(name)
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data := line[len("data:"):]
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			if len(o.data) > 0 {
				o.data = append(o.data, '\n')
			}
			o.data = append(o.data, data...)
		}
		o.line = o.line[:0]
	}
}

func semanticDelta(data []byte, eventType string) bool {
	var event struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	if event.Type == "" {
		event.Type = eventType
	}
	if strings.HasPrefix(event.Type, "response.") && strings.HasSuffix(event.Type, ".delta") {
		var delta string
		if json.Unmarshal(event.Delta, &delta) == nil && delta != "" {
			return true
		}
	}
	for _, choice := range event.Choices {
		d := choice.Delta
		if d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" {
			return true
		}
		for _, call := range d.ToolCalls {
			if call.Function.Name != "" || call.Function.Arguments != "" {
				return true
			}
		}
	}
	return false
}
