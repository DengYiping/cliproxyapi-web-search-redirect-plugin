package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// turnRunner streams one model turn and calls onEvent with each Responses event payload.
type turnRunner func(ctx context.Context, body []byte, onEvent func([]byte) error) error

// searchRunner runs one web search query.
type searchRunner func(ctx context.Context, query string) (searchResult, error)

// eventSink delivers one Responses event to the client.
type eventSink func(eventType string, data []byte) error

const searchLimitOutput = "The web search limit for this request has been reached. Answer with the information already gathered."

// redirectLoop runs the client model, executes its web_search function calls on the search
// backend, and feeds the results back until the model answers. The client sees a single
// Responses stream in which each search appears as a web_search_call item.
type redirectLoop struct {
	body        []byte
	model       string
	clientTools []byte
	maxSearches int
	runTurn     turnRunner
	search      searchRunner
	sink        eventSink
	cache       *searchCache
	debug       bool

	started      bool
	seq          int64
	nextIndex    int64
	responseID   string
	createdAt    int64
	output       []string
	usage        usageTotals
	searchesDone int
	final        []byte
	failure      error
}

type turnState struct {
	number         int
	indexMap       map[int64]int64
	hidden         map[int64]bool
	items          []string
	calls          []searchCall
	clientToolCall bool
	terminal       string
	response       gjson.Result
}

type searchCall struct {
	callID string
	query  string
}

func (l *redirectLoop) run(ctx context.Context) error {
	body := l.body
	for turn := 0; ; turn++ {
		t := &turnState{number: turn, indexMap: map[int64]int64{}, hidden: map[int64]bool{}}
		if errTurn := l.runTurn(ctx, body, func(data []byte) error { return l.handleEvent(t, data) }); errTurn != nil {
			return errTurn
		}
		switch t.terminal {
		case "":
			return fmt.Errorf("model stream ended before a terminal event")
		case "response.failed", "error":
			return nil
		}
		// The turn cap is a backstop for a model that keeps searching after the limit reply.
		if len(t.calls) == 0 || turn > l.maxSearches {
			return l.finish(t)
		}
		outputs, errSearch := l.runSearches(ctx, t.calls)
		if errSearch != nil {
			return errSearch
		}
		// Client tool calls end the response; the client replays the searches next turn.
		if t.clientToolCall {
			return l.finish(t)
		}
		var errAppend error
		if body, errAppend = appendTurn(body, t.turnItems(), outputs); errAppend != nil {
			return errAppend
		}
	}
}

// runNonStreaming runs the loop and returns the final response object.
func (l *redirectLoop) runNonStreaming(ctx context.Context) ([]byte, error) {
	if errRun := l.run(ctx); errRun != nil {
		return nil, errRun
	}
	if l.failure != nil {
		return nil, l.failure
	}
	if l.final == nil {
		return nil, fmt.Errorf("model produced no response")
	}
	return l.final, nil
}

func (l *redirectLoop) handleEvent(t *turnState, data []byte) error {
	event := gjson.ParseBytes(data)
	eventType := event.Get("type").String()
	switch eventType {
	case "response.created", "response.in_progress":
		if t.number > 0 {
			return nil
		}
		if eventType == "response.created" && !l.started {
			l.started = true
			l.responseID = event.Get("response.id").String()
			l.createdAt = event.Get("response.created_at").Int()
		}
		return l.emit(eventType, data, nil)
	case "response.output_item.added":
		index := event.Get("output_index").Int()
		if isSearchFunctionCall(event.Get("item")) {
			t.hidden[index] = true
			return nil
		}
		return l.emit(eventType, data, l.clientIndex(t, index))
	case "response.output_item.done":
		index := event.Get("output_index").Int()
		item := event.Get("item")
		t.items = append(t.items, item.Raw)
		if t.hidden[index] || isSearchFunctionCall(item) {
			t.hidden[index] = true
			t.calls = append(t.calls, parseSearchCall(item))
			return nil
		}
		if strings.HasSuffix(item.Get("type").String(), "_call") {
			t.clientToolCall = true
		}
		l.output = append(l.output, item.Raw)
		return l.emit(eventType, data, l.clientIndex(t, index))
	case "response.completed", "response.incomplete":
		t.terminal = eventType
		t.response = event.Get("response")
		l.usage.add(t.response.Get("usage"))
		return nil
	case "response.failed", "error":
		t.terminal = eventType
		l.failure = fmt.Errorf("model request failed: %s", firstNonEmpty(event.Get("response.error.message").String(), event.Get("message").String(), event.Raw))
		return l.emit(eventType, data, nil)
	}
	if indexResult := event.Get("output_index"); indexResult.Exists() {
		index := indexResult.Int()
		if t.hidden[index] {
			return nil
		}
		return l.emit(eventType, data, l.clientIndex(t, index))
	}
	return l.emit(eventType, data, nil)
}

// runSearches executes the turn's search calls, streams a web_search_call item for each,
// and returns the function_call_output items for the next model turn.
func (l *redirectLoop) runSearches(ctx context.Context, calls []searchCall) ([]string, error) {
	outputs := make([]string, 0, len(calls))
	for _, call := range calls {
		if l.searchesDone >= l.maxSearches {
			outputs = append(outputs, functionCallOutputItem(call.callID, searchLimitOutput))
			continue
		}
		l.searchesDone++
		output, errSearch := l.runSearch(ctx, call)
		if errSearch != nil {
			return nil, errSearch
		}
		outputs = append(outputs, functionCallOutputItem(call.callID, output))
	}
	return outputs, nil
}

func (l *redirectLoop) runSearch(ctx context.Context, call searchCall) (string, error) {
	itemID := "ws_" + randomHex(12)
	index := l.nextIndex
	l.nextIndex++
	item := webSearchCallItem(itemID, "in_progress", call.query, nil)
	if errEmit := l.emitNew("response.output_item.added", map[string]any{"output_index": index, "item": rawJSON(item)}); errEmit != nil {
		return "", errEmit
	}
	for _, eventType := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching"} {
		if errEmit := l.emitNew(eventType, map[string]any{"output_index": index, "item_id": itemID}); errEmit != nil {
			return "", errEmit
		}
	}

	result, errSearch := l.search(ctx, call.query)
	status := "completed"
	output := result.Text
	if errSearch != nil {
		if errors.Is(errSearch, context.Canceled) {
			return "", errSearch
		}
		status = "failed"
		output = "The web search failed: " + errSearch.Error()
		hostLog("warn", pluginName+": search failed", map[string]any{"query": call.query, "error": errSearch.Error()})
	} else if l.debug {
		hostLog("info", pluginName+": search completed", map[string]any{"query": call.query, "sources": len(result.Sources)})
	}
	l.cache.put(itemID, output)

	if errEmit := l.emitNew("response.web_search_call.completed", map[string]any{"output_index": index, "item_id": itemID}); errEmit != nil {
		return "", errEmit
	}
	item = webSearchCallItem(itemID, status, call.query, result.Sources)
	l.output = append(l.output, item)
	if errEmit := l.emitNew("response.output_item.done", map[string]any{"output_index": index, "item": rawJSON(item)}); errEmit != nil {
		return "", errEmit
	}
	return output, nil
}

// finish emits the terminal event with the merged output and usage of every turn.
func (l *redirectLoop) finish(t *turnState) error {
	response := []byte(t.response.Raw)
	if !t.response.IsObject() {
		response = []byte(`{"object":"response"}`)
	}
	if !l.started {
		// The model never sent response.created; open the stream before closing it.
		l.responseID = firstNonEmpty(gjson.GetBytes(response, "id").String(), "resp_"+randomHex(12))
		l.createdAt = time.Now().Unix()
		if errStart := l.emitNew("response.created", map[string]any{"response": map[string]any{
			"id": l.responseID, "object": "response", "created_at": l.createdAt, "status": "in_progress", "model": l.model, "output": []any{},
		}}); errStart != nil {
			return errStart
		}
		l.started = true
	}
	var errSet error
	if l.responseID != "" {
		if response, errSet = sjson.SetBytes(response, "id", l.responseID); errSet != nil {
			return errSet
		}
	}
	if l.createdAt > 0 {
		if response, errSet = sjson.SetBytes(response, "created_at", l.createdAt); errSet != nil {
			return errSet
		}
	}
	if response, errSet = sjson.SetRawBytes(response, "output", []byte("["+strings.Join(l.output, ",")+"]")); errSet != nil {
		return errSet
	}
	// Report the tools the client sent, not the function tool that replaced hosted search.
	if len(l.clientTools) > 0 {
		if response, errSet = sjson.SetRawBytes(response, "tools", l.clientTools); errSet != nil {
			return errSet
		}
	}
	if usage := l.usage.json(); usage != nil {
		if response, errSet = sjson.SetRawBytes(response, "usage", usage); errSet != nil {
			return errSet
		}
	}
	l.final = response
	return l.emitNew(t.terminal, map[string]any{"response": rawJSON(string(response))})
}

func (l *redirectLoop) clientIndex(t *turnState, upstream int64) *int64 {
	index, ok := t.indexMap[upstream]
	if !ok {
		index = l.nextIndex
		l.nextIndex++
		t.indexMap[upstream] = index
	}
	return &index
}

// emit forwards an upstream event with a fresh sequence number and a remapped output_index.
func (l *redirectLoop) emit(eventType string, data []byte, outputIndex *int64) error {
	out, errSet := sjson.SetBytes(data, "sequence_number", l.seq)
	if errSet != nil {
		return errSet
	}
	if outputIndex != nil {
		if out, errSet = sjson.SetBytes(out, "output_index", *outputIndex); errSet != nil {
			return errSet
		}
	}
	l.seq++
	return l.sink(eventType, out)
}

// emitNew emits an event the plugin creates itself.
func (l *redirectLoop) emitNew(eventType string, fields map[string]any) error {
	data := []byte(`{}`)
	var errSet error
	if data, errSet = sjson.SetBytes(data, "type", eventType); errSet != nil {
		return errSet
	}
	for key, value := range fields {
		if raw, ok := value.(rawJSON); ok {
			data, errSet = sjson.SetRawBytes(data, key, []byte(raw))
		} else {
			data, errSet = sjson.SetBytes(data, key, value)
		}
		if errSet != nil {
			return errSet
		}
	}
	return l.emit(eventType, data, nil)
}

func (t *turnState) turnItems() []string {
	// response.completed carries the authoritative output list; the done events are a fallback.
	if output := t.response.Get("output"); output.IsArray() && len(output.Array()) > 0 {
		items := make([]string, 0, len(output.Array()))
		for _, item := range output.Array() {
			items = append(items, item.Raw)
		}
		return items
	}
	return t.items
}

type rawJSON string

func isSearchFunctionCall(item gjson.Result) bool {
	return item.Get("type").String() == "function_call" && item.Get("name").String() == searchToolName
}

func parseSearchCall(item gjson.Result) searchCall {
	args := item.Get("arguments").String()
	query := strings.TrimSpace(gjson.Get(args, "query").String())
	if query == "" {
		query = strings.TrimSpace(args)
	}
	return searchCall{callID: firstNonEmpty(item.Get("call_id").String(), item.Get("id").String()), query: query}
}

func webSearchCallItem(id, status, query string, sources []string) string {
	item := []byte(`{"type":"web_search_call"}`)
	item, _ = sjson.SetBytes(item, "id", id)
	item, _ = sjson.SetBytes(item, "status", status)
	item, _ = sjson.SetBytes(item, "action.type", "search")
	item, _ = sjson.SetBytes(item, "action.query", query)
	if len(sources) > 0 {
		list := make([]map[string]string, 0, len(sources))
		for _, url := range sources {
			list = append(list, map[string]string{"type": "url", "url": url})
		}
		item, _ = sjson.SetBytes(item, "action.sources", list)
	}
	return string(item)
}

// usageTotals sums Responses usage across model turns.
type usageTotals struct {
	seen            bool
	inputTokens     int64
	cachedTokens    int64
	outputTokens    int64
	reasoningTokens int64
	totalTokens     int64
}

func (u *usageTotals) add(usage gjson.Result) {
	if !usage.IsObject() {
		return
	}
	u.seen = true
	u.inputTokens += usage.Get("input_tokens").Int()
	u.cachedTokens += usage.Get("input_tokens_details.cached_tokens").Int()
	u.outputTokens += usage.Get("output_tokens").Int()
	u.reasoningTokens += usage.Get("output_tokens_details.reasoning_tokens").Int()
	u.totalTokens += usage.Get("total_tokens").Int()
}

func (u *usageTotals) json() []byte {
	if !u.seen {
		return nil
	}
	total := u.totalTokens
	if total == 0 {
		total = u.inputTokens + u.outputTokens
	}
	return []byte(fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,"output_tokens_details":{"reasoning_tokens":%d},"total_tokens":%d}`,
		u.inputTokens, u.cachedTokens, u.outputTokens, u.reasoningTokens, total))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
