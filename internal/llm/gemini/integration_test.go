package gemini_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/llm/gemini"
)

// These tests talk to the real Gemini API. They are skipped unless
// GEMINI_API_KEY is set, so the suite still runs offline and in CI.
//
// They exist because the unit tests can only prove what this code sends. Only
// the service can prove what it accepts, and the schemas involved are the ones
// an MCP server actually publishes, not ones written to be convenient.

// mcpTool mirrors the tool shape returned by an MCP server's tools/list.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func liveProvider(t *testing.T) *gemini.Provider {
	t.Helper()
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY is not set")
	}

	provider, err := gemini.New(context.Background(), gemini.Config{
		Model: os.Getenv("GEMINI_MODEL"),
	})
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}
	return provider
}

// filesystemTools loads the tool definitions captured from a real run of the
// official @modelcontextprotocol/server-filesystem server.
func filesystemTools(t *testing.T) []llm.ToolDef {
	t.Helper()

	raw, err := os.ReadFile("testdata/filesystem-tools.json")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	var tools []mcpTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode testdata: %v", err)
	}

	defs := make([]llm.ToolDef, 0, len(tools))
	for _, tool := range tools {
		defs = append(defs, llm.ToolDef{
			// Namespaced exactly as the host will send them once several
			// servers are connected at once.
			Name:        "filesystem__" + tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return defs
}

const systemPrompt = "You are a file assistant. Use the tools available to you."

func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The question this whole adapter rests on: does Gemini accept draft-07
// schemas, exactly as MCP publishes them, through ParametersJsonSchema?
func TestLiveAcceptsRealMCPSchemas(t *testing.T) {
	provider := liveProvider(t)
	tools := filesystemTools(t)

	resp, err := provider.Generate(liveContext(t), llm.Request{
		System:   systemPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "What is 2 + 2?"}},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("Gemini rejected %d MCP tool schemas: %v", len(tools), err)
	}
	t.Logf("accepted %d schemas; model answered %q (in %d tok, out %d tok)",
		len(tools), resp.Text, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

// Accepting the schemas is not enough: the model has to pick the right tool and
// fill its arguments from a schema it has never seen before.
func TestLiveRequestsToolWithArguments(t *testing.T) {
	provider := liveProvider(t)

	resp, err := provider.Generate(liveContext(t), llm.Request{
		System: "You are a file assistant. Use the tools available to you.",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Text: "List the files in the directory /home/user/project.",
		}},
		Tools: filesystemTools(t),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !resp.WantsTools() {
		t.Fatalf("model asked for no tool; it replied %q", resp.Text)
	}

	call := resp.ToolCalls[0]
	t.Logf("model called %s with %v", call.Name, call.Arguments)

	if call.Name != "filesystem__list_directory" &&
		call.Name != "filesystem__list_directory_with_sizes" &&
		call.Name != "filesystem__directory_tree" {
		t.Errorf("model chose %q, want a directory listing tool", call.Name)
	}
	if _, present := call.Arguments["path"]; !present {
		t.Errorf("call carries no path argument: %v", call.Arguments)
	}
}

// requestListing drives the first turn and returns the model's real tool call.
//
// A tool call cannot be fabricated for these models: the signature that comes
// back with a call has to be replayed with it, and only the model can issue
// one. Any test that continues a conversation has to start it for real.
func requestListing(t *testing.T, provider *gemini.Provider, ctx context.Context,
	tools []llm.ToolDef) ([]llm.Message, llm.ToolCall) {
	t.Helper()

	history := []llm.Message{{
		Role: llm.RoleUser,
		Text: "List the files in the directory /home/user/project.",
	}}

	first, err := provider.Generate(ctx, llm.Request{
		System:   systemPrompt,
		Messages: history,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if !first.WantsTools() {
		t.Skipf("model requested no tool; it replied %q", first.Text)
	}

	call := first.ToolCalls[0]
	if len(call.ProviderState) == 0 {
		t.Logf("warning: the call carries no provider state; replaying it may be rejected")
	}

	history = append(history, llm.Message{
		Role:      llm.RoleModel,
		Text:      first.Text,
		ToolCalls: first.ToolCalls,
	})
	return history, call
}

// The full agent round trip: the model asks for a tool, the host answers, and
// the model uses that answer. This is the loop the chatbot is built on, and it
// is the turn where a dropped signature shows up.
func TestLiveCompletesToolRoundTrip(t *testing.T) {
	provider := liveProvider(t)
	tools := filesystemTools(t)
	ctx := liveContext(t)

	history, call := requestListing(t, provider, ctx, tools)
	history = append(history, llm.Message{
		Role: llm.RoleUser,
		ToolResults: []llm.ToolResult{{
			ID:      call.ID,
			Name:    call.Name,
			Content: "[FILE] main.go\n[FILE] go.mod\n[DIR] internal",
		}},
	})

	second, err := provider.Generate(ctx, llm.Request{
		System:   systemPrompt,
		Messages: history,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if second.Text == "" {
		t.Error("model produced no answer after the tool result")
	}
	t.Logf("model concluded: %q", second.Text)
}

// A failed tool must reach the model as something it can recover from, not as
// a transport error that ends the conversation.
func TestLiveHandlesFailedToolResult(t *testing.T) {
	provider := liveProvider(t)
	tools := filesystemTools(t)
	ctx := liveContext(t)

	history, call := requestListing(t, provider, ctx, tools)
	history = append(history, llm.Message{
		Role: llm.RoleUser,
		ToolResults: []llm.ToolResult{{
			ID:      call.ID,
			Name:    call.Name,
			Content: "ENOENT: no such file or directory, open '/home/user/project'",
			IsError: true,
		}},
	})

	resp, err := provider.Generate(ctx, llm.Request{
		System:   systemPrompt,
		Messages: history,
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("a failed tool result broke the conversation: %v", err)
	}
	t.Logf("model responded to the failure: %q", resp.Text)
	if resp.Text == "" && !resp.WantsTools() {
		t.Error("model neither explained the failure nor tried another tool")
	}
}
