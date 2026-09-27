package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostError is a failed host callback, keeping the HTTP status the host reported.
type hostError struct {
	code    string
	message string
	status  int
}

func (e *hostError) Error() string {
	if e.code == "" {
		return e.message
	}
	return e.code + ": " + e.message
}

// statusError is an upstream failure with a known HTTP status.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func httpStatusFromError(err error) int {
	var hostErr *hostError
	if errors.As(err, &hostErr) && hostErr.status > 0 {
		return hostErr.status
	}
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		return statusErr.status
	}
	return 0
}

type hostModelRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type hostLogRequest struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// hostExecFunc runs a non-streaming model request through the host.
type hostExecFunc func(req pluginapi.HostModelExecutionRequest, hostCallbackID string) ([]byte, error)

func hostModelExecute(req pluginapi.HostModelExecutionRequest, hostCallbackID string) ([]byte, error) {
	req.Stream = false
	raw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelRequest{HostModelExecutionRequest: req, HostCallbackID: hostCallbackID})
	if errCall != nil {
		return nil, errCall
	}
	var resp pluginapi.HostModelExecutionResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return nil, fmt.Errorf("decode host model response: %w", errDecode)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &statusError{status: resp.StatusCode, err: fmt.Errorf("model %s returned status %d: %s", req.Model, resp.StatusCode, truncate(string(resp.Body), 300))}
	}
	return resp.Body, nil
}

// hostTurnRunner streams one Responses turn of the client model through the host.
// The host skips this plugin's router for the nested call, so it reaches the real provider.
func hostTurnRunner(hostCallbackID, model string, headers http.Header) turnRunner {
	return func(_ context.Context, body []byte, onEvent func([]byte) error) error {
		raw, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelRequest{
			HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
				EntryProtocol: responsesFormat,
				ExitProtocol:  responsesFormat,
				Model:         model,
				Stream:        true,
				Body:          body,
				Headers:       headers,
			},
			HostCallbackID: hostCallbackID,
		})
		if errCall != nil {
			return errCall
		}
		var resp pluginapi.HostModelStreamResponse
		if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
			return fmt.Errorf("decode host model stream response: %w", errDecode)
		}
		if strings.TrimSpace(resp.StreamID) != "" {
			defer closeHostModelStream(resp.StreamID)
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return &statusError{status: resp.StatusCode, err: fmt.Errorf("model %s returned status %d", model, resp.StatusCode)}
		}
		if strings.TrimSpace(resp.StreamID) == "" {
			return fmt.Errorf("host model stream returned no stream_id")
		}

		parser := &sseParser{onData: onEvent}
		for {
			chunkRaw, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
			if errRead != nil {
				return errRead
			}
			var chunk pluginapi.HostModelStreamReadResponse
			if errDecode := json.Unmarshal(chunkRaw, &chunk); errDecode != nil {
				return fmt.Errorf("decode host model stream chunk: %w", errDecode)
			}
			if chunk.Error != "" {
				return errors.New(chunk.Error)
			}
			if errFeed := parser.feed(chunk.Payload); errFeed != nil {
				return errFeed
			}
			if chunk.Done {
				return parser.flush()
			}
		}
	}
}

// forwardAlt sends a side-endpoint request (such as responses/compact) to the client model
// without the hosted search tool, which the model's provider cannot run.
func forwardAlt(exec pluginapi.ExecutorRequest, hostCallbackID string, body []byte) ([]byte, int, error) {
	stripped, errStrip := stripHostedWebSearch(body)
	if errStrip != nil {
		return nil, http.StatusBadRequest, errStrip
	}
	payload, errExec := hostModelExecute(pluginapi.HostModelExecutionRequest{
		EntryProtocol: responsesFormat,
		ExitProtocol:  responsesFormat,
		Model:         exec.Model,
		Body:          stripped,
		Headers:       exec.Headers,
		Alt:           exec.Alt,
	}, hostCallbackID)
	if errExec != nil {
		return nil, httpStatusFromError(errExec), errExec
	}
	return payload, http.StatusOK, nil
}

func closeHostModelStream(streamID string) {
	_, _ = callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: streamID, Payload: bytes.Clone(payload)})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}

func hostLog(level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: message, Fields: fields})
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
