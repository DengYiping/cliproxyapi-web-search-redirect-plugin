package main

import (
	"bytes"
	"encoding/json"
)

// sseParser turns host stream chunks into Responses event payloads. A chunk may hold
// whole events, single lines, or a final line without its trailing newline.
type sseParser struct {
	pending []byte
	onData  func([]byte) error
}

func (p *sseParser) feed(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	// pending only ever holds a partial line. A partial data line is an unfinished JSON
	// payload, while other fields (event:, id:) are complete once the next field starts.
	if len(p.pending) > 0 && !endsWithNewline(p.pending) && !isDataLine(p.pending) && startsSSEField(chunk) {
		p.pending = append(p.pending, '\n')
	}
	p.pending = append(p.pending, chunk...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		line := p.pending[:i]
		p.pending = p.pending[i+1:]
		if errLine := p.line(line); errLine != nil {
			return errLine
		}
	}
	if payload, ok := completeDataPayload(p.pending); ok {
		p.pending = nil
		return p.onData(payload)
	}
	return nil
}

func (p *sseParser) flush() error {
	line := p.pending
	p.pending = nil
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	return p.line(line)
}

func (p *sseParser) line(line []byte) error {
	if payload, ok := completeDataPayload(line); ok {
		return p.onData(payload)
	}
	return nil
}

// completeDataPayload returns the JSON payload of a single SSE data line. Responses events
// are single-line JSON, so each data line is one event. Bare JSON lines are accepted too.
func completeDataPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	} else if !bytes.HasPrefix(trimmed, []byte("{")) {
		return nil, false
	}
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return nil, false
	}
	return bytes.Clone(trimmed), true
}

func isDataLine(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:"))
}

func endsWithNewline(b []byte) bool {
	return len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r')
}

func startsSSEField(chunk []byte) bool {
	trimmed := bytes.TrimLeft(chunk, " \t")
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(trimmed, []byte(prefix)) {
			return true
		}
	}
	return false
}

func formatSSE(eventType string, data []byte) []byte {
	out := make([]byte, 0, len(eventType)+len(data)+16)
	out = append(out, "event: "...)
	out = append(out, eventType...)
	out = append(out, "\ndata: "...)
	out = append(out, data...)
	out = append(out, "\n\n"...)
	return out
}
