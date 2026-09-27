package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const searchResponse = `{"id":"resp_s","object":"response","status":"completed","output":[
	{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go 1.26","sources":[{"type":"url","url":"https://go.dev/blog"}]}},
	{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.26 was released ([go.dev](https://go.dev/doc/go1.26)).","annotations":[{"type":"url_citation","url":"https://go.dev/doc/go1.26","title":"Go 1.26"}]}]}
]}`

func TestParseSearchResponse(t *testing.T) {
	result, errParse := parseSearchResponse([]byte(searchResponse))
	if errParse != nil {
		t.Fatalf("parseSearchResponse: %v", errParse)
	}
	if strings.Join(result.Sources, ",") != "https://go.dev/blog,https://go.dev/doc/go1.26" {
		t.Fatalf("sources = %v", result.Sources)
	}
	if !strings.HasPrefix(result.Text, "Go 1.26 was released") {
		t.Fatalf("text = %q", result.Text)
	}
	// Only sources missing from the summary are appended.
	if !strings.Contains(result.Text, "Sources:\n- https://go.dev/blog") || strings.Count(result.Text, "https://go.dev/doc/go1.26") != 1 {
		t.Fatalf("text = %q", result.Text)
	}
}

func TestParseSearchResponseFromSSE(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + strings.ReplaceAll(searchResponse, "\n", "") + "}\n\n"
	result, errParse := parseSearchResponse([]byte(stream))
	if errParse != nil {
		t.Fatalf("parseSearchResponse: %v", errParse)
	}
	if len(result.Sources) != 2 {
		t.Fatalf("sources = %v", result.Sources)
	}
}

func TestParseSearchResponseErrors(t *testing.T) {
	if _, errParse := parseSearchResponse([]byte(`{"output":[]}`)); errParse == nil {
		t.Fatal("empty output should fail")
	}
	if _, errParse := parseSearchResponse([]byte(`{"error":{"message":"quota exceeded"}}`)); errParse == nil || !strings.Contains(errParse.Error(), "quota exceeded") {
		t.Fatalf("error = %v", errParse)
	}
}

func TestOpenAISearcherBuildsHostRequest(t *testing.T) {
	var captured pluginapi.HostModelExecutionRequest
	var callbackID string
	searcher := &openAISearcher{
		cfg:            config{SearchModel: "gpt-5.5", SearchProvider: "codex", SearchReasoningEffort: "low"},
		tool:           []byte(`{"type":"web_search","external_web_access":true,"filters":{"allowed_domains":["go.dev"]}}`),
		hostCallbackID: "cb-1",
		exec: func(req pluginapi.HostModelExecutionRequest, hostCallbackID string) ([]byte, error) {
			captured = req
			callbackID = hostCallbackID
			return []byte(searchResponse), nil
		},
	}
	result, errSearch := searcher.search(context.Background(), "go 1.26")
	if errSearch != nil {
		t.Fatalf("search: %v", errSearch)
	}
	if len(result.Sources) != 2 {
		t.Fatalf("sources = %v", result.Sources)
	}
	if captured.Model != "gpt-5.5" || captured.ForcedProvider != "codex" || captured.EntryProtocol != "openai-response" || captured.Stream || callbackID != "cb-1" {
		t.Fatalf("host request = %+v, callback = %q", captured, callbackID)
	}
	body := gjson.ParseBytes(captured.Body)
	if body.Get("tools.0.filters.allowed_domains.0").String() != "go.dev" || !body.Get("tools.0.external_web_access").Bool() {
		t.Fatalf("search tool = %s", body.Get("tools").Raw)
	}
	if body.Get("input.0.content.0.text").String() != "Search the web for: go 1.26" || body.Get("reasoning.effort").String() != "low" {
		t.Fatalf("search body = %s", captured.Body)
	}
}

func TestOpenAISearcherWrapsHostError(t *testing.T) {
	searcher := &openAISearcher{
		cfg: config{SearchModel: "gpt-5.5"},
		exec: func(pluginapi.HostModelExecutionRequest, string) ([]byte, error) {
			return nil, &hostError{code: "host_call_failed", message: "rate limited", status: 429}
		},
	}
	_, errSearch := searcher.search(context.Background(), "q")
	var hostErr *hostError
	if !errors.As(errSearch, &hostErr) || httpStatusFromError(errSearch) != 429 {
		t.Fatalf("error = %v", errSearch)
	}
}

func TestSearchCacheEvictsOldest(t *testing.T) {
	cache := newSearchCache(2)
	cache.put("a", "1")
	cache.put("b", "2")
	cache.put("c", "3")
	if _, ok := cache.get("a"); ok {
		t.Fatal("oldest entry should be evicted")
	}
	if got, ok := cache.get("c"); !ok || got != "3" {
		t.Fatalf("newest entry = %q, %v", got, ok)
	}
}
