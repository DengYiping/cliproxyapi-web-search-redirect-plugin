package main

import (
	"strings"
	"testing"
)

func collectSSE(t *testing.T, chunks ...string) []string {
	t.Helper()
	var got []string
	parser := &sseParser{onData: func(data []byte) error {
		got = append(got, string(data))
		return nil
	}}
	for _, chunk := range chunks {
		if errFeed := parser.feed([]byte(chunk)); errFeed != nil {
			t.Fatalf("feed: %v", errFeed)
		}
	}
	if errFlush := parser.flush(); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	return got
}

func TestSSEParserChunkShapes(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{"whole events", []string{"event: a\ndata: {\"n\":1}\n\nevent: b\ndata: {\"n\":2}\n\n"}},
		{"events without trailing newline", []string{"event: a\ndata: {\"n\":1}", "event: b\ndata: {\"n\":2}"}},
		{"one line per chunk", []string{"event: a", "data: {\"n\":1}", "", "event: b", "data: {\"n\":2}"}},
		{"data split mid-json", []string{"data: {\"n\"", ":1}\n", "data: {\"n\":2}\n\n"}},
		{"crlf and done marker", []string{"data: {\"n\":1}\r\n\r\ndata: {\"n\":2}\r\n\r\ndata: [DONE]\r\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectSSE(t, tt.chunks...)
			if strings.Join(got, "|") != `{"n":1}|{"n":2}` {
				t.Fatalf("payloads = %v", got)
			}
		})
	}
}

func TestFormatSSE(t *testing.T) {
	if got := string(formatSSE("response.created", []byte(`{"a":1}`))); got != "event: response.created\ndata: {\"a\":1}\n\n" {
		t.Fatalf("formatSSE = %q", got)
	}
}
