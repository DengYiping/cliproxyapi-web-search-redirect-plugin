package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type recordedEvent struct {
	eventType string
	data      gjson.Result
}

// scriptedTurns replays one list of Responses events per model turn through the SSE parser,
// the way the host delivers them, and records each request body.
type scriptedTurns struct {
	turns  [][]string
	bodies [][]byte
}

func (s *scriptedTurns) run(_ context.Context, body []byte, onEvent func([]byte) error) error {
	if len(s.bodies) >= len(s.turns) {
		return errors.New("unexpected extra model turn")
	}
	events := s.turns[len(s.bodies)]
	s.bodies = append(s.bodies, append([]byte(nil), body...))
	parser := &sseParser{onData: onEvent}
	for _, event := range events {
		eventType := gjson.Get(event, "type").String()
		if errFeed := parser.feed([]byte("event: " + eventType + "\ndata: " + event)); errFeed != nil {
			return errFeed
		}
	}
	return parser.flush()
}

func newTestLoop(t *testing.T, turns [][]string, search searchRunner, maxSearches int) (*redirectLoop, *scriptedTurns, *[]recordedEvent) {
	t.Helper()
	clientBody := []byte(`{"model":"kimi-k2","input":"What changed in Go 1.26?","tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"shell","parameters":{"type":"object"}}],"stream":true}`)
	body, errPrepare := prepareModelRequest(clientBody, "kimi-k2", nil)
	if errPrepare != nil {
		t.Fatalf("prepareModelRequest: %v", errPrepare)
	}
	script := &scriptedTurns{turns: turns}
	var events []recordedEvent
	loop := &redirectLoop{
		body:        body,
		model:       "kimi-k2",
		clientTools: []byte(gjson.GetBytes(clientBody, "tools").Raw),
		maxSearches: maxSearches,
		runTurn:     script.run,
		search:      search,
		cache:       newSearchCache(16),
		sink: func(eventType string, data []byte) error {
			events = append(events, recordedEvent{eventType: eventType, data: gjson.ParseBytes(data)})
			return nil
		},
	}
	return loop, script, &events
}

func eventTypes(events []recordedEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.eventType)
	}
	return types
}

const (
	turnCreated   = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_first","object":"response","created_at":100,"status":"in_progress","output":[]}}`
	turnCreated2  = `{"type":"response.created","sequence_number":0,"response":{"id":"resp_second","object":"response","created_at":200,"status":"in_progress","output":[]}}`
	turnProgress2 = `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_second","object":"response","created_at":200,"status":"in_progress","output":[]}}`
	searchAdded   = `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_call_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"web_search"}}`
	searchDelta   = `{"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc_call_1","delta":"{\"query\":\"go 1.26 release notes\"}"}`
	searchDone    = `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"fc_call_1","type":"function_call","status":"completed","arguments":"{\"query\":\"go 1.26 release notes\"}","call_id":"call_1","name":"web_search"}}`
	searchTurnEnd = `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_first","object":"response","created_at":100,"status":"completed","output":[{"id":"fc_call_1","type":"function_call","status":"completed","arguments":"{\"query\":\"go 1.26 release notes\"}","call_id":"call_1","name":"web_search"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
	msgAdded      = `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"}}`
	msgDelta      = `{"type":"response.output_text.delta","sequence_number":3,"output_index":0,"content_index":0,"item_id":"msg_1","delta":"Go 1.26 shipped."}`
	msgDone       = `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Go 1.26 shipped.","annotations":[]}]}}`
	msgTurnEnd    = `{"type":"response.completed","sequence_number":5,"response":{"id":"resp_second","object":"response","created_at":200,"status":"completed","model":"kimi-k2","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Go 1.26 shipped.","annotations":[]}]}],"usage":{"input_tokens":20,"output_tokens":7,"total_tokens":27,"input_tokens_details":{"cached_tokens":4}}}}`
)

func fixedSearch(queries *[]string) searchRunner {
	return func(_ context.Context, query string) (searchResult, error) {
		*queries = append(*queries, query)
		return searchResult{Text: "Go 1.26 was released in February 2026.", Sources: []string{"https://go.dev/doc/go1.26"}}, nil
	}
}

func TestLoopRedirectsSearchAndStitchesStream(t *testing.T) {
	var queries []string
	loop, script, events := newTestLoop(t, [][]string{
		{turnCreated, searchAdded, searchDelta, searchDone, searchTurnEnd},
		{turnCreated2, turnProgress2, msgAdded, msgDelta, msgDone, msgTurnEnd},
	}, fixedSearch(&queries), 5)

	if errRun := loop.run(context.Background()); errRun != nil {
		t.Fatalf("run: %v", errRun)
	}

	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.web_search_call.in_progress",
		"response.web_search_call.searching",
		"response.web_search_call.completed",
		"response.output_item.done",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if got := eventTypes(*events); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types:\n got %v\nwant %v", got, wantTypes)
	}
	for i, event := range *events {
		if seq := event.data.Get("sequence_number").Int(); seq != int64(i) {
			t.Fatalf("event %d sequence_number = %d", i, seq)
		}
	}
	if got := queries; len(got) != 1 || got[0] != "go 1.26 release notes" {
		t.Fatalf("queries = %v", got)
	}

	ev := *events
	searchItem := ev[1].data.Get("item")
	if searchItem.Get("type").String() != "web_search_call" || ev[1].data.Get("output_index").Int() != 0 {
		t.Fatalf("search item added = %s", ev[1].data.Raw)
	}
	doneItem := ev[5].data.Get("item")
	if doneItem.Get("status").String() != "completed" || doneItem.Get("action.sources.0.url").String() != "https://go.dev/doc/go1.26" {
		t.Fatalf("search item done = %s", doneItem.Raw)
	}
	for _, i := range []int{6, 7, 8} {
		if idx := ev[i].data.Get("output_index").Int(); idx != 1 {
			t.Fatalf("event %d output_index = %d, want 1", i, idx)
		}
	}

	final := ev[9].data.Get("response")
	if final.Get("id").String() != "resp_first" || final.Get("created_at").Int() != 100 {
		t.Fatalf("final response identity = %s", final.Raw)
	}
	if types := final.Get("output.#.type").Raw; types != `["web_search_call","message"]` {
		t.Fatalf("final output types = %s", types)
	}
	if tools := final.Get("tools.#.type").Raw; tools != `["web_search","function"]` {
		t.Fatalf("final tools = %s", tools)
	}
	usage := final.Get("usage")
	if usage.Get("input_tokens").Int() != 30 || usage.Get("output_tokens").Int() != 12 || usage.Get("total_tokens").Int() != 42 || usage.Get("input_tokens_details.cached_tokens").Int() != 4 {
		t.Fatalf("usage = %s", usage.Raw)
	}

	if len(script.bodies) != 2 {
		t.Fatalf("model turns = %d, want 2", len(script.bodies))
	}
	second := gjson.ParseBytes(script.bodies[1])
	if types := second.Get("input.#.type").Raw; types != `["message","function_call","function_call_output"]` {
		t.Fatalf("second turn input types = %s", types)
	}
	if out := second.Get("input.2"); out.Get("call_id").String() != "call_1" || !strings.Contains(out.Get("output").String(), "February 2026") {
		t.Fatalf("function_call_output = %s", out.Raw)
	}
	if cached, ok := loop.cache.get(searchItem.Get("id").String()); !ok || !strings.Contains(cached, "February 2026") {
		t.Fatalf("cache entry = %q, %v", cached, ok)
	}
}

func TestLoopEndsResponseWhenClientToolIsCalled(t *testing.T) {
	var queries []string
	shellAdded := `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_call_2","type":"function_call","status":"in_progress","arguments":"","call_id":"call_2","name":"shell"}}`
	shellDone := `{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_call_2","type":"function_call","status":"completed","arguments":"{\"cmd\":\"go version\"}","call_id":"call_2","name":"shell"}}`
	end := `{"type":"response.completed","response":{"id":"resp_first","status":"completed","output":[]}}`
	loop, script, events := newTestLoop(t, [][]string{
		{turnCreated, searchAdded, searchDone, shellAdded, shellDone, end},
	}, fixedSearch(&queries), 5)

	if errRun := loop.run(context.Background()); errRun != nil {
		t.Fatalf("run: %v", errRun)
	}
	if len(script.bodies) != 1 || len(queries) != 1 {
		t.Fatalf("turns = %d, searches = %d; want 1 and 1", len(script.bodies), len(queries))
	}
	ev := *events
	final := ev[len(ev)-1].data.Get("response")
	if types := final.Get("output.#.type").Raw; types != `["function_call","web_search_call"]` {
		t.Fatalf("final output types = %s", types)
	}
	if name := final.Get("output.0.name").String(); name != "shell" {
		t.Fatalf("client tool call name = %q", name)
	}
}

func TestLoopStopsSearchingAtLimit(t *testing.T) {
	var queries []string
	search2Added := strings.ReplaceAll(searchAdded, "call_1", "call_9")
	search2Done := strings.ReplaceAll(searchDone, "call_1", "call_9")
	search2End := strings.ReplaceAll(searchTurnEnd, "call_1", "call_9")
	loop, script, events := newTestLoop(t, [][]string{
		{turnCreated, searchAdded, searchDone, searchTurnEnd},
		{turnCreated2, search2Added, search2Done, search2End},
		{turnCreated2, msgAdded, msgDone, msgTurnEnd},
	}, fixedSearch(&queries), 1)

	if errRun := loop.run(context.Background()); errRun != nil {
		t.Fatalf("run: %v", errRun)
	}
	if len(queries) != 1 || len(script.bodies) != 3 {
		t.Fatalf("searches = %d, turns = %d; want 1 and 3", len(queries), len(script.bodies))
	}
	third := gjson.ParseBytes(script.bodies[2])
	if last := third.Get("input.@reverse.0.output").String(); last != searchLimitOutput {
		t.Fatalf("limit output = %q", last)
	}
	ev := *events
	if types := ev[len(ev)-1].data.Get("response.output.#.type").Raw; types != `["web_search_call","message"]` {
		t.Fatalf("final output types = %s", types)
	}
}

func TestLoopReportsSearchFailureToModel(t *testing.T) {
	loop, script, events := newTestLoop(t, [][]string{
		{turnCreated, searchAdded, searchDone, searchTurnEnd},
		{turnCreated2, msgAdded, msgDone, msgTurnEnd},
	}, func(context.Context, string) (searchResult, error) {
		return searchResult{}, errors.New("status 429")
	}, 5)

	if errRun := loop.run(context.Background()); errRun != nil {
		t.Fatalf("run: %v", errRun)
	}
	output := gjson.GetBytes(script.bodies[1], "input.2.output").String()
	if !strings.Contains(output, "The web search failed") {
		t.Fatalf("function_call_output = %q", output)
	}
	if status := (*events)[5].data.Get("item.status").String(); status != "failed" {
		t.Fatalf("web_search_call status = %q", status)
	}
}

func TestLoopNonStreamingReturnsFinalResponse(t *testing.T) {
	var queries []string
	loop, _, _ := newTestLoop(t, [][]string{
		{turnCreated, searchAdded, searchDone, searchTurnEnd},
		{turnCreated2, msgAdded, msgDone, msgTurnEnd},
	}, fixedSearch(&queries), 5)

	final, errRun := loop.runNonStreaming(context.Background())
	if errRun != nil {
		t.Fatalf("runNonStreaming: %v", errRun)
	}
	if got := gjson.GetBytes(final, "output.1.content.0.text").String(); got != "Go 1.26 shipped." {
		t.Fatalf("final text = %q in %s", got, final)
	}
}

func TestLoopForwardsFailedTurn(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_first","status":"failed","error":{"code":"server_error","message":"upstream exploded"}}}`
	loop, _, events := newTestLoop(t, [][]string{{turnCreated, failed}}, nil, 5)

	if _, errRun := loop.runNonStreaming(context.Background()); errRun == nil || !strings.Contains(errRun.Error(), "upstream exploded") {
		t.Fatalf("runNonStreaming error = %v", errRun)
	}
	if got := eventTypes(*events); strings.Join(got, ",") != "response.created,response.failed" {
		t.Fatalf("event types = %v", got)
	}
}
