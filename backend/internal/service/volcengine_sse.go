package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var errVolcengineSSEEventTooLarge = errors.New("Ark SSE event exceeds size limit")

// Normalize SSE framing before the existing protocol parsers. Enabled only by
// the Ark account's transport wrapper; no background goroutine or full-response
// buffering is introduced. A frame is released as soon as its blank line arrives.
type volcengineSSEBody struct {
	io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
	err     error
	started bool
	limit   int
}

func newVolcengineSSEBody(body io.ReadCloser) io.ReadCloser {
	return newVolcengineSSEBodyWithLimit(body, defaultMaxLineSize)
}

func newVolcengineSSEBodyWithLimit(body io.ReadCloser, limit int) io.ReadCloser {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, min(64*1024, limit)), limit)
	scanner.Split(splitVolcengineSSELine)
	return &volcengineSSEBody{ReadCloser: body, scanner: scanner, limit: limit}
}

func splitVolcengineSSELine(data []byte, atEOF bool) (int, []byte, error) {
	for i, ch := range data {
		if ch == '\n' {
			return i + 1, data[:i], nil
		}
		if ch == '\r' {
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (b *volcengineSSEBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 && b.err == nil {
		b.pending, b.err = b.frame()
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *volcengineSSEBody) frame() ([]byte, error) {
	var headers, data []string
	event := ""
	size := 0
	for {
		if !b.scanner.Scan() {
			// The SSE standard discards an event without its terminating blank
			// line. Do not make a partial terminal JSON look like completion.
			if err := b.scanner.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		line := b.scanner.Text()
		if !b.started {
			line = strings.TrimPrefix(line, "\ufeff")
			b.started = true
		}
		size += len(line)
		if size > b.limit {
			return nil, errVolcengineSSEEventTooLarge
		}
		if line == "" {
			var output bytes.Buffer
			for _, header := range headers {
				_, _ = output.WriteString(header)
				_ = output.WriteByte('\n')
			}
			if event != "" {
				_, _ = output.WriteString(event)
				_ = output.WriteByte('\n')
			}
			if len(data) > 0 {
				// JSON event data permits line breaks between tokens. Compact
				// only JSON, preserving non-JSON sentinels and unknown payloads.
				payload := []byte(strings.Join(data, "\n"))
				var compact bytes.Buffer
				if json.Compact(&compact, payload) == nil {
					payload = compact.Bytes()
				} else if len(data) > 1 {
					for _, line := range data {
						_, _ = output.WriteString("data: ")
						_, _ = output.WriteString(line)
						_ = output.WriteByte('\n')
					}
					_ = output.WriteByte('\n')
					return output.Bytes(), nil
				}
				_, _ = output.WriteString("data: ")
				_, _ = output.Write(payload)
				_ = output.WriteByte('\n')
			}
			_ = output.WriteByte('\n')
			return output.Bytes(), nil
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		} else if field == "event" {
			event = "event: " + strings.TrimPrefix(value, " ")
		} else {
			headers = append(headers, line)
		}
	}
}
