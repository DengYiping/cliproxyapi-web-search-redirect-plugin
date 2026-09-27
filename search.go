package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const searchInstructions = "You are a web research assistant. Use the web_search tool to research the user's query, then reply with a concise, factual summary of the most relevant findings, including specific details such as versions, dates, and numbers. Cite sources inline as markdown links. Do not answer from memory or speculate beyond the search results."

type searchResult struct {
	Text    string
	Sources []string
}

// openAISearcher runs one query on an OpenAI model with the hosted web_search tool.
type openAISearcher struct {
	cfg            config
	tool           []byte
	hostCallbackID string
	exec           hostExecFunc
}

func (s *openAISearcher) search(_ context.Context, query string) (searchResult, error) {
	body, errBuild := buildSearchRequest(s.cfg, query, s.tool)
	if errBuild != nil {
		return searchResult{}, errBuild
	}
	raw, errExec := s.exec(pluginapi.HostModelExecutionRequest{
		EntryProtocol:  responsesFormat,
		ExitProtocol:   responsesFormat,
		Model:          s.cfg.SearchModel,
		Body:           body,
		ForcedProvider: s.cfg.SearchProvider,
	}, s.hostCallbackID)
	if errExec != nil {
		return searchResult{}, fmt.Errorf("search via %s: %w", s.cfg.SearchModel, errExec)
	}
	return parseSearchResponse(raw)
}

// buildSearchRequest builds the Responses request sent to the search model. The client's
// hosted tool definition is reused so options like filters and user_location carry over.
func buildSearchRequest(cfg config, query string, tool []byte) ([]byte, error) {
	if !gjson.ValidBytes(tool) || gjson.GetBytes(tool, "type").String() == "" {
		tool = []byte(`{"type":"web_search"}`)
	}
	body := []byte(`{"stream":false,"store":false,"tool_choice":"auto"}`)
	var errSet error
	steps := []struct {
		path  string
		value any
		raw   []byte
	}{
		{path: "model", value: cfg.SearchModel},
		{path: "instructions", value: searchInstructions},
		{path: "input", raw: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)},
		{path: "input.0.content.0.text", value: "Search the web for: " + query},
		{path: "tools", raw: []byte("[" + string(tool) + "]")},
	}
	for _, step := range steps {
		if step.raw != nil {
			body, errSet = sjson.SetRawBytes(body, step.path, step.raw)
		} else {
			body, errSet = sjson.SetBytes(body, step.path, step.value)
		}
		if errSet != nil {
			return nil, fmt.Errorf("build search request (%s): %w", step.path, errSet)
		}
	}
	if cfg.SearchReasoningEffort != "" {
		if body, errSet = sjson.SetBytes(body, "reasoning.effort", cfg.SearchReasoningEffort); errSet != nil {
			return nil, fmt.Errorf("build search request (reasoning): %w", errSet)
		}
	}
	return body, nil
}

// parseSearchResponse extracts the summary text and cited URLs from a Responses result.
// It accepts a response object, a response.completed event, or a buffered SSE stream.
func parseSearchResponse(raw []byte) (searchResult, error) {
	response := gjson.ParseBytes(raw)
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] != '{' {
		response = completedResponseFromSSE(trimmed)
	} else if nested := response.Get("response"); nested.IsObject() {
		response = nested
	}
	if msg := response.Get("error.message").String(); msg != "" {
		return searchResult{}, fmt.Errorf("search model error: %s", msg)
	}

	var texts []string
	var sources []string
	seen := map[string]bool{}
	addSource := func(url string) {
		if url != "" && !seen[url] {
			seen[url] = true
			sources = append(sources, url)
		}
	}
	for _, item := range response.Get("output").Array() {
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				if text := strings.TrimSpace(part.Get("text").String()); text != "" {
					texts = append(texts, text)
				}
				for _, annotation := range part.Get("annotations").Array() {
					addSource(annotation.Get("url").String())
				}
			}
		case "web_search_call":
			for _, source := range item.Get("action.sources").Array() {
				addSource(source.Get("url").String())
			}
		}
	}
	text := strings.Join(texts, "\n\n")
	if text == "" && len(sources) == 0 {
		return searchResult{}, fmt.Errorf("search model returned no results")
	}
	var missing []string
	for _, url := range sources {
		if !strings.Contains(text, url) {
			missing = append(missing, "- "+url)
		}
	}
	if len(missing) > 0 {
		text = strings.TrimSpace(text + "\n\nSources:\n" + strings.Join(missing, "\n"))
	}
	return searchResult{Text: text, Sources: sources}, nil
}

func completedResponseFromSSE(raw []byte) gjson.Result {
	var completed gjson.Result
	parser := &sseParser{onData: func(data []byte) error {
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = gjson.GetBytes(data, "response")
		}
		return nil
	}}
	_ = parser.feed(raw)
	_ = parser.flush()
	return completed
}

// searchCache keeps search outputs by web_search_call item ID so later turns can give the
// model the same results the client only sees as a web_search_call item.
type searchCache struct {
	mu      sync.Mutex
	limit   int
	entries map[string]string
	order   []string
}

var searchHistory = newSearchCache(4096)

func newSearchCache(limit int) *searchCache {
	return &searchCache{limit: limit, entries: make(map[string]string)}
}

func (c *searchCache) get(id string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	output, ok := c.entries[id]
	return output, ok
}

func (c *searchCache) put(id, output string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[id]; !exists {
		c.order = append(c.order, id)
	}
	c.entries[id] = output
	for len(c.order) > c.limit {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}
