package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	pluginName    = "web-search-redirect"
	pluginVersion = "0.1.0"

	// responsesFormat is the CLIProxyAPI protocol key for the OpenAI Responses API.
	responsesFormat = "openai-response"
)

// githubRepository is required non-empty by the host; override with
// -ldflags "-X main.githubRepository=<url>" when publishing elsewhere.
var githubRepository = "https://github.com/DengYiping/cliproxyapi-web-search-redirect-plugin"

var currentConfig atomic.Pointer[config]

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func main() {}

func init() {
	cfg := defaultConfig()
	currentConfig.Store(&cfg)
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), 0))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginName})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errParse := parseConfig(req.ConfigYAML)
	if errParse != nil {
		hostLog("warn", pluginName+": invalid config", map[string]any{"error": errParse.Error()})
		return errParse
	}
	currentConfig.Store(&cfg)
	return nil
}

func loadedConfig() config {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "DengYiping",
			GitHubRepository: githubRepository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Case-insensitive wildcard patterns of models without hosted web search (for example \"kimi-*\"). Empty matches nothing."},
				{Name: "search_model", Type: pluginapi.ConfigFieldTypeString, Description: "OpenAI model that runs each web search with the hosted web_search tool."},
				{Name: "search_provider", Type: pluginapi.ConfigFieldTypeString, Description: "Built-in provider forced for search requests (default codex). Empty lets the host resolve it."},
				{Name: "search_reasoning_effort", Type: pluginapi.ConfigFieldTypeString, Description: "Reasoning effort for search requests. Empty keeps the provider default."},
				{Name: "max_searches", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum searches per client request (default 5)."},
				{Name: "debug_log", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Log routing decisions and searches through the host logger."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{responsesFormat},
			ExecutorOutputFormats: []string{responsesFormat},
		},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model route request: %w", errUnmarshal)
	}
	cfg := loadedConfig()
	if !shouldRedirect(cfg, req.SourceFormat, req.RequestedModel, req.Body) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if cfg.DebugLog {
		hostLog("info", pluginName+": redirecting web search", map[string]any{"model": req.RequestedModel, "search_model": cfg.SearchModel})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "responses_web_search_redirect",
	})
}

// shouldRedirect reports whether a request is a Responses call to a configured model
// that asks for the hosted web search tool.
func shouldRedirect(cfg config, sourceFormat, model string, body []byte) bool {
	if !strings.EqualFold(strings.TrimSpace(sourceFormat), responsesFormat) {
		return false
	}
	if !matchesModel(cfg.Models, model) {
		return false
	}
	_, found := findHostedWebSearchTool(body)
	return found
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	cfg := loadedConfig()
	body := executorBody(req.ExecutorRequest)
	if strings.TrimSpace(req.Alt) != "" {
		// Side endpoints such as responses/compact have no tool loop; drop the hosted
		// search tool and forward the call unchanged otherwise.
		payload, status, errForward := forwardAlt(req.ExecutorRequest, req.HostCallbackID, body)
		if errForward != nil {
			return errorEnvelope("executor_error", errForward.Error(), status), nil
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: payload, Headers: jsonHeaders()})
	}
	loop, errLoop := newHostLoop(cfg, req.ExecutorRequest, req.HostCallbackID, body, func(string, []byte) error { return nil })
	if errLoop != nil {
		return errorEnvelope("executor_error", errLoop.Error(), http.StatusBadRequest), nil
	}
	final, errRun := loop.runNonStreaming(context.Background())
	if errRun != nil {
		return errorEnvelope("executor_error", errRun.Error(), httpStatusFromError(errRun)), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: final, Headers: jsonHeaders()})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 0), nil
	}
	cfg := loadedConfig()
	body := executorBody(req.ExecutorRequest)
	loop, errLoop := newHostLoop(cfg, req.ExecutorRequest, req.HostCallbackID, body, func(eventType string, data []byte) error {
		return emitPluginStreamChunk(streamID, formatSSE(eventType, data))
	})
	if errLoop != nil {
		return errorEnvelope("executor_error", errLoop.Error(), http.StatusBadRequest), nil
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("web search redirect panic: %v", recovered))
			}
		}()
		if errRun := loop.run(context.Background()); errRun != nil {
			hostLog("warn", pluginName+": stream failed", map[string]any{"model": req.Model, "error": errRun.Error()})
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// newHostLoop wires a redirect loop to host model execution for one client request.
func newHostLoop(cfg config, exec pluginapi.ExecutorRequest, hostCallbackID string, body []byte, sink eventSink) (*redirectLoop, error) {
	model := strings.TrimSpace(exec.Model)
	if model == "" {
		model = strings.TrimSpace(jsonString(body, "model"))
	}
	tool, _ := findHostedWebSearchTool(body)
	prepared, errPrepare := prepareModelRequest(body, model, searchHistory)
	if errPrepare != nil {
		return nil, errPrepare
	}
	searcher := &openAISearcher{
		cfg:            cfg,
		tool:           []byte(tool.Raw),
		hostCallbackID: hostCallbackID,
		exec:           hostModelExecute,
	}
	return &redirectLoop{
		body:        prepared,
		model:       model,
		clientTools: []byte(gjson.GetBytes(body, "tools").Raw),
		maxSearches: cfg.MaxSearches,
		runTurn:     hostTurnRunner(hostCallbackID, model, exec.Headers),
		search:      searcher.search,
		sink:        sink,
		cache:       searchHistory,
		debug:       cfg.DebugLog,
	}, nil
}

func executorBody(exec pluginapi.ExecutorRequest) []byte {
	if len(exec.Payload) > 0 {
		return exec.Payload
	}
	return exec.OriginalRequest
}

func jsonHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback and returns the decoded envelope result.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostError{code: env.Error.Code, message: env.Error.Message, status: env.Error.HTTPStatus}
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
