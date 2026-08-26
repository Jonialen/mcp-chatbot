package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

func testServer(t *testing.T) *Server {
	t.Helper()

	server := New("test-server", "1.0.0")
	server.Register(Tool{
		Name:        "echo",
		Description: "Return what it was given.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var params struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", err
			}
			return params.Text, nil
		},
	})
	server.Register(Tool{
		Name:        "always_fails",
		Description: "Fail on purpose.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New("the record was not found")
		},
	})
	return server
}

func request(t *testing.T, id int, method string, params any) *jsonrpc.Message {
	t.Helper()
	msg, err := jsonrpc.NewRequest(int64(id), method, params)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return msg
}

func decodeResult[T any](t *testing.T, msg *jsonrpc.Message) T {
	t.Helper()

	if msg == nil {
		t.Fatal("server produced no response")
	}
	if msg.Error != nil {
		t.Fatalf("server answered with an error: %v", msg.Error)
	}

	var out T
	if err := json.Unmarshal(msg.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", msg.Result, err)
	}
	return out
}

func TestInitializeAnnouncesToolsCapability(t *testing.T) {
	server := testServer(t)
	ctx := context.Background()

	resp := server.Handle(ctx, request(t, 1, mcp.MethodInitialize, mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
	}))

	result := decodeResult[mcp.InitializeResult](t, resp)
	if !result.SupportsTools() {
		t.Error("the server did not announce the tools capability")
	}
	if result.ServerInfo.Name != "test-server" {
		t.Errorf("serverInfo.name = %q", result.ServerInfo.Name)
	}
}

// A client built against an earlier revision speaks a tools surface this server
// understands, so it is answered in its own version rather than turned away.
func TestInitializeEchoesTheClientsVersion(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(),
		request(t, 1, mcp.MethodInitialize, map[string]any{"protocolVersion": "2024-11-05"}))

	result := decodeResult[mcp.InitializeResult](t, resp)
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want the client's own", result.ProtocolVersion)
	}
}

func TestListToolsPublishesSchemasVerbatim(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(), request(t, 2, mcp.MethodListTools, nil))
	result := decodeResult[mcp.ListToolsResult](t, resp)

	if len(result.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(result.Tools))
	}
	// Sorted, so the list is identical between calls.
	if result.Tools[0].Name != "always_fails" || result.Tools[1].Name != "echo" {
		t.Errorf("tools are not in a stable order: %+v", result.Tools)
	}

	var schema map[string]any
	if err := json.Unmarshal(result.Tools[1].InputSchema, &schema); err != nil {
		t.Fatalf("the published schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema was altered on the way out: %v", schema)
	}
}

func TestCallToolReturnsOutput(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(), request(t, 3, mcp.MethodCallTool, mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	}))

	result := decodeResult[mcp.CallToolResult](t, resp)
	if result.IsError {
		t.Errorf("IsError = true for a successful call: %s", result.Text())
	}
	if result.Text() != "hello" {
		t.Errorf("Text() = %q", result.Text())
	}
}

// A tool that ran and failed is reported inside a successful response, so the
// model can read the failure and work around it.
func TestFailingToolIsReportedInsideASuccessfulResponse(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(), request(t, 4, mcp.MethodCallTool, mcp.CallToolParams{
		Name:      "always_fails",
		Arguments: map[string]any{},
	}))

	if resp.Error != nil {
		t.Fatalf("a failing tool was reported as a protocol error: %v", resp.Error)
	}

	result := decodeResult[mcp.CallToolResult](t, resp)
	if !result.IsError {
		t.Error("IsError = false for a tool that failed")
	}
	if !strings.Contains(result.Text(), "not found") {
		t.Errorf("the failure was not passed on: %q", result.Text())
	}
}

// Asking for a tool the server never published is a protocol error: the client
// asked for something that does not exist.
func TestUnknownToolIsAProtocolError(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(), request(t, 5, mcp.MethodCallTool, mcp.CallToolParams{
		Name: "invented",
	}))

	if resp.Error == nil {
		t.Fatal("an unknown tool did not produce an error")
	}
	if resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeInvalidParams)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	server := testServer(t)

	resp := server.Handle(context.Background(), request(t, 6, "resources/list", nil))
	if resp.Error == nil {
		t.Fatal("an unknown method did not produce an error")
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeMethodNotFound)
	}
}

// Answering a notification is a protocol violation, not a courtesy.
func TestNotificationsAreNotAnswered(t *testing.T) {
	server := testServer(t)

	msg, err := jsonrpc.NewNotification(mcp.MethodInitialized, nil)
	if err != nil {
		t.Fatalf("build notification: %v", err)
	}
	if resp := server.Handle(context.Background(), msg); resp != nil {
		t.Errorf("the server answered a notification: %+v", resp)
	}
}

func TestRegisterRejectsDuplicateNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering the same tool twice was allowed")
		}
	}()

	server := New("test", "1.0.0")
	add := Tool{
		Name:        "same",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}
	server.Register(add)
	server.Register(add)
}

func TestServeStdioAnswersFramesInOrder(t *testing.T) {
	server := testServer(t)

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`not json at all`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
	}, "\n") + "\n")

	var out strings.Builder
	if err := server.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// initialize, tools/list, the parse error, tools/call. The notification is
	// not answered.
	if len(lines) != 4 {
		t.Fatalf("server wrote %d frames, want 4:\n%s", len(lines), out.String())
	}

	for i, line := range lines {
		var msg jsonrpc.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("frame %d is not valid JSON: %s", i, line)
		}
		if msg.JSONRPC != jsonrpc.Version {
			t.Errorf("frame %d has version %q", i, msg.JSONRPC)
		}
	}

	// A frame that will not parse carries no id, so the error uses a null one.
	var parseError jsonrpc.Message
	if err := json.Unmarshal([]byte(lines[2]), &parseError); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parseError.Error == nil || parseError.Error.Code != jsonrpc.CodeParseError {
		t.Errorf("malformed frame produced %+v", parseError)
	}
	if string(parseError.ID) != "null" {
		t.Errorf("parse error id = %s, want null", parseError.ID)
	}
}

func TestServeStdioStopsAtEndOfInput(t *testing.T) {
	server := testServer(t)

	var out strings.Builder
	err := server.ServeStdio(context.Background(), strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("ServeStdio on empty input = %v, want a clean stop", err)
	}
	if out.Len() != 0 {
		t.Errorf("the server wrote %q with nothing to answer", out.String())
	}
}

func ExampleServer() {
	server := New("example", "1.0.0")
	server.Register(Tool{
		Name:        "greet",
		Description: "Greet somebody by name.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var params struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", err
			}
			return "Hello, " + params.Name, nil
		},
	})

	msg, _ := jsonrpc.NewRequest(1, mcp.MethodCallTool, mcp.CallToolParams{
		Name:      "greet",
		Arguments: map[string]any{"name": "world"},
	})
	fmt.Println(string(server.Handle(context.Background(), msg).Result))
	// Output: {"content":[{"text":"Hello, world","type":"text"}],"isError":false}
}
