// Package mcp implements the Model Context Protocol adapters that run
// inbound JSON-RPC tool calls through the potent pipeline.
//
// Two transports are supported: Streamable HTTP (jsonrpc.go's Message
// types over net/http) and stdio (line-delimited JSON-RPC over a child
// process's stdin/stdout).
package mcp

import (
	"encoding/json"
	"errors"
)

// JSON-RPC 2.0 method names we care about for idempotency. Everything else
// flows through untouched.
const (
	MethodToolsCall = "tools/call"
)

// Message is a single JSON-RPC 2.0 frame. JSON-RPC permits requests,
// responses, notifications, and errors; we use a single union struct so
// raw frames round-trip cleanly through the proxy.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether the message represents a client→server request
// (has both a Method and an ID).
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether the message is a one-way notification
// (Method without ID).
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether the message is a response (no Method).
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ToolsCallParams matches the MCP tools/call params shape:
//
//	{"name": "send_email", "arguments": { ... }}
type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseToolsCall extracts the tool name and raw arguments JSON from a
// tools/call request message.
func ParseToolsCall(m *Message) (string, []byte, error) {
	if m.Method != MethodToolsCall {
		return "", nil, errors.New("not a tools/call message")
	}
	var p ToolsCallParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return "", nil, err
	}
	if p.Name == "" {
		return "", nil, errors.New("tools/call missing name")
	}
	return p.Name, []byte(p.Arguments), nil
}

// NewToolsCallResponse builds a JSON-RPC response carrying a cached result
// for the given request id.
func NewToolsCallResponse(id json.RawMessage, cachedResult []byte) Message {
	return Message{
		JSONRPC: "2.0",
		ID:      id,
		Result:  cachedResult,
	}
}

// NewErrorResponse builds a JSON-RPC error response.
func NewErrorResponse(id json.RawMessage, code int, msg string) Message {
	return Message{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
}
