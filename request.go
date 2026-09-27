package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// searchToolName is the function tool the client model calls in place of hosted search.
const searchToolName = "web_search"

const searchToolDefinition = `{"type":"function","name":"web_search","description":"Search the web for current information. Returns a summary of the results with source URLs. Use it when the answer depends on recent events, documentation, or facts you are unsure about.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"The search query."}},"required":["query"],"additionalProperties":false}}`

var hostedWebSearchTypes = map[string]bool{
	"web_search":                    true,
	"web_search_preview":            true,
	"web_search_preview_2025_03_11": true,
	"web_search_2025_08_26":         true,
}

func isHostedWebSearchTool(tool gjson.Result) bool {
	return hostedWebSearchTypes[tool.Get("type").String()]
}

// findHostedWebSearchTool returns the first hosted web search tool in a Responses request.
func findHostedWebSearchTool(body []byte) (gjson.Result, bool) {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if isHostedWebSearchTool(tool) {
			return tool, true
		}
	}
	return gjson.Result{}, false
}

// prepareModelRequest rewrites a client Responses request for a model without hosted search:
// the hosted tool becomes a function tool, and earlier web_search_call items become
// function call pairs the model can read.
func prepareModelRequest(body []byte, model string, cache *searchCache) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	out, errStrip := stripHostedWebSearch(body)
	if errStrip != nil {
		return nil, errStrip
	}
	tools := []string{searchToolDefinition}
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		tools = append(tools, tool.Raw)
	}
	var errSet error
	if out, errSet = sjson.SetRawBytes(out, "tools", []byte("["+strings.Join(tools, ",")+"]")); errSet != nil {
		return nil, fmt.Errorf("set tools: %w", errSet)
	}
	if isHostedWebSearchTool(gjson.GetBytes(body, "tool_choice")) {
		if out, errSet = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"function","name":"web_search"}`)); errSet != nil {
			return nil, fmt.Errorf("set tool_choice: %w", errSet)
		}
	}
	if input := gjson.GetBytes(out, "input"); input.IsArray() {
		if out, errSet = sjson.SetRawBytes(out, "input", rehydrateSearchHistory(input, cache)); errSet != nil {
			return nil, fmt.Errorf("set input: %w", errSet)
		}
	}
	if model != "" {
		if out, errSet = sjson.SetBytes(out, "model", model); errSet != nil {
			return nil, fmt.Errorf("set model: %w", errSet)
		}
	}
	if out, errSet = sjson.SetBytes(out, "stream", true); errSet != nil {
		return nil, fmt.Errorf("set stream: %w", errSet)
	}
	return out, nil
}

// stripHostedWebSearch removes hosted web search tools and a tool_choice that names one.
func stripHostedWebSearch(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}
	kept := make([]string, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		if !isHostedWebSearchTool(tool) {
			kept = append(kept, tool.Raw)
		}
	}
	out, errSet := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(kept, ",")+"]"))
	if errSet != nil {
		return nil, fmt.Errorf("strip hosted web search: %w", errSet)
	}
	if isHostedWebSearchTool(gjson.GetBytes(out, "tool_choice")) {
		if out, errSet = sjson.DeleteBytes(out, "tool_choice"); errSet != nil {
			return nil, fmt.Errorf("strip web search tool_choice: %w", errSet)
		}
	}
	return out, nil
}

// rehydrateSearchHistory replaces web_search_call items from earlier turns with a
// function_call and function_call_output pair holding the cached search results.
func rehydrateSearchHistory(input gjson.Result, cache *searchCache) []byte {
	items := make([]string, 0, len(input.Array()))
	for i, item := range input.Array() {
		if item.Get("type").String() != "web_search_call" {
			items = append(items, item.Raw)
			continue
		}
		callID := item.Get("id").String()
		if callID == "" {
			callID = fmt.Sprintf("ws_history_%d", i)
		}
		query := webSearchCallQuery(item)
		output, found := cache.get(callID)
		if !found {
			output = uncachedSearchOutput(item, query)
		}
		items = append(items, functionCallItem(callID, query), functionCallOutputItem(callID, output))
	}
	return []byte("[" + strings.Join(items, ",") + "]")
}

func webSearchCallQuery(item gjson.Result) string {
	if query := item.Get("action.query").String(); query != "" {
		return query
	}
	return item.Get("action.queries.0").String()
}

func uncachedSearchOutput(item gjson.Result, query string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Results for the earlier web search %q are no longer available.", query)
	if sources := item.Get("action.sources").Array(); len(sources) > 0 {
		b.WriteString(" Sources it returned:")
		for _, source := range sources {
			if url := source.Get("url").String(); url != "" {
				b.WriteString("\n- " + url)
			}
		}
	}
	return b.String()
}

func functionCallItem(callID, query string) string {
	args, _ := json.Marshal(map[string]string{"query": query})
	raw, _ := json.Marshal(map[string]string{
		"type":      "function_call",
		"call_id":   callID,
		"name":      searchToolName,
		"arguments": string(args),
	})
	return string(raw)
}

func functionCallOutputItem(callID, output string) string {
	raw, _ := json.Marshal(map[string]string{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  output,
	})
	return string(raw)
}

// appendTurn adds one model turn's output items and the search results to the request input.
func appendTurn(body []byte, turnItems, outputs []string) ([]byte, error) {
	var items []string
	input := gjson.GetBytes(body, "input")
	switch {
	case input.IsArray():
		for _, item := range input.Array() {
			items = append(items, item.Raw)
		}
	case input.Type == gjson.String:
		msg, _ := json.Marshal(map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": input.String()}},
		})
		items = append(items, string(msg))
	}
	items = append(items, turnItems...)
	items = append(items, outputs...)
	out, errSet := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(items, ",")+"]"))
	if errSet != nil {
		return nil, fmt.Errorf("append turn to input: %w", errSet)
	}
	// A forced search choice would loop forever once the first search is answered.
	if gjson.GetBytes(out, "tool_choice.name").String() == searchToolName {
		if out, errSet = sjson.DeleteBytes(out, "tool_choice"); errSet != nil {
			return nil, fmt.Errorf("reset tool_choice: %w", errSet)
		}
	}
	return out, nil
}

func jsonString(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}
