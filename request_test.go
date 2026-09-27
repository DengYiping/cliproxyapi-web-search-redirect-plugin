package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestShouldRedirect(t *testing.T) {
	cfg := config{Models: []string{"kimi-*", "*/deepseek-*", "glm-4.6"}}
	withSearch := []byte(`{"tools":[{"type":"function","name":"shell"},{"type":"web_search"}]}`)
	withoutSearch := []byte(`{"tools":[{"type":"function","name":"web_search"}]}`)
	tests := []struct {
		name   string
		format string
		model  string
		body   []byte
		want   bool
	}{
		{"matching model with hosted search", "openai-response", "kimi-k2", withSearch, true},
		{"pattern is case-insensitive", "openai-response", "Kimi-K2", withSearch, true},
		{"slash in model", "openai-response", "together/deepseek-v3", withSearch, true},
		{"exact pattern", "openai-response", "glm-4.6", withSearch, true},
		{"unlisted model", "openai-response", "gpt-5.5", withSearch, false},
		{"no hosted search tool", "openai-response", "kimi-k2", withoutSearch, false},
		{"chat completions source", "openai", "kimi-k2", withSearch, false},
		{"claude source", "claude", "kimi-k2", withSearch, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRedirect(cfg, tt.format, tt.model, tt.body); got != tt.want {
				t.Fatalf("shouldRedirect() = %v, want %v", got, tt.want)
			}
		})
	}
	if shouldRedirect(config{}, "openai-response", "kimi-k2", withSearch) {
		t.Fatal("empty models list should match nothing")
	}
}

func TestPrepareModelRequestRewritesToolsAndHistory(t *testing.T) {
	cache := newSearchCache(8)
	cache.put("ws_cached", "Cached summary with https://example.com")
	body := []byte(`{
		"model":"alias",
		"stream":false,
		"tool_choice":{"type":"web_search"},
		"tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"web_search_call","id":"ws_cached","status":"completed","action":{"type":"search","query":"cached query"}},
			{"type":"web_search_call","id":"ws_openai","status":"completed","action":{"type":"search","query":"old query","sources":[{"type":"url","url":"https://go.dev"}]}}
		]
	}`)
	out, errPrepare := prepareModelRequest(body, "kimi-k2", cache)
	if errPrepare != nil {
		t.Fatalf("prepareModelRequest: %v", errPrepare)
	}
	root := gjson.ParseBytes(out)
	if got := root.Get("model").String(); got != "kimi-k2" {
		t.Fatalf("model = %q", got)
	}
	if !root.Get("stream").Bool() {
		t.Fatal("stream should be forced to true")
	}
	if got := root.Get("tools.#.name").Raw; got != `["web_search","shell"]` {
		t.Fatalf("tool names = %s", got)
	}
	if got := root.Get("tools.#.type").Raw; got != `["function","function"]` {
		t.Fatalf("tool types = %s", got)
	}
	if got := root.Get("tool_choice").Raw; got != `{"type":"function","name":"web_search"}` {
		t.Fatalf("tool_choice = %s", got)
	}
	if got := root.Get("input.#.type").Raw; got != `["message","function_call","function_call_output","function_call","function_call_output"]` {
		t.Fatalf("input types = %s", got)
	}
	if got := root.Get("input.1.arguments").String(); got != `{"query":"cached query"}` {
		t.Fatalf("rehydrated arguments = %s", got)
	}
	if got := root.Get("input.2.output").String(); got != "Cached summary with https://example.com" {
		t.Fatalf("cached output = %q", got)
	}
	uncached := root.Get("input.4.output").String()
	if !strings.Contains(uncached, "no longer available") || !strings.Contains(uncached, "https://go.dev") {
		t.Fatalf("uncached output = %q", uncached)
	}
	if root.Get("input.3.call_id").String() != root.Get("input.4.call_id").String() {
		t.Fatal("rehydrated call and output must share a call_id")
	}
}

func TestStripHostedWebSearch(t *testing.T) {
	out, errStrip := stripHostedWebSearch([]byte(`{"tools":[{"type":"web_search_preview"},{"type":"function","name":"shell"}],"tool_choice":{"type":"web_search_preview"}}`))
	if errStrip != nil {
		t.Fatalf("stripHostedWebSearch: %v", errStrip)
	}
	if got := gjson.GetBytes(out, "tools.#.name").Raw; got != `["shell"]` {
		t.Fatalf("tools = %s", got)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatal("tool_choice naming hosted search should be removed")
	}
}

func TestAppendTurnConvertsStringInputAndResetsForcedChoice(t *testing.T) {
	body := []byte(`{"input":"question","tool_choice":{"type":"function","name":"web_search"}}`)
	out, errAppend := appendTurn(body, []string{`{"type":"function_call","call_id":"c1","name":"web_search","arguments":"{}"}`}, []string{functionCallOutputItem("c1", "result")})
	if errAppend != nil {
		t.Fatalf("appendTurn: %v", errAppend)
	}
	if got := gjson.GetBytes(out, "input.#.type").Raw; got != `["message","function_call","function_call_output"]` {
		t.Fatalf("input types = %s", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "question" {
		t.Fatalf("user text = %q", got)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatal("forced search tool_choice should be removed after the first search")
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, errParse := parseConfig([]byte("models: [\" kimi-* \", \"\"]\nsearch_provider: \" Codex \"\n"))
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	if len(cfg.Models) != 1 || cfg.Models[0] != "kimi-*" {
		t.Fatalf("models = %v", cfg.Models)
	}
	if cfg.SearchModel != defaultSearchModel || cfg.SearchProvider != "codex" || cfg.MaxSearches != defaultMaxSearches || cfg.SearchReasoningEffort != "low" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"kimi-*", "kimi-k2", true},
		{"*", "anything", true},
		{"a*b*c", "axxbyyc", true},
		{"a*a", "a", false},
		{"ab*b", "abb", true},
		{"qwen3", "qwen3-coder", false},
	}
	for _, tt := range tests {
		if got := wildcardMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}
