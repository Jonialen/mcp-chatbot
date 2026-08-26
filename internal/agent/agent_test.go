package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

// scriptedProvider replays a fixed list of responses, one per turn, and records
// the conversation it was given each time.
type scriptedProvider struct {
	responses []llm.Response
	err       error

	mu       sync.Mutex
	turn     int
	requests []llm.Request
}

func (p *scriptedProvider) Name() string  { return "scripted" }
func (p *scriptedProvider) Model() string { return "scripted-1" }

func (p *scriptedProvider) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.err != nil {
		return nil, p.err
	}
	p.requests = append(p.requests, req)

	if p.turn >= len(p.responses) {
		return &llm.Response{Text: "done"}, nil
	}
	response := p.responses[p.turn]
	p.turn++
	return &response, nil
}

func (p *scriptedProvider) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider was never called")
	}
	return p.requests[len(p.requests)-1]
}

// fakeTools stands in for the registry.
type fakeTools struct {
	defs    []llm.ToolDef
	replies map[string]*mcp.CallToolResult
	errs    map[string]error
	delay   time.Duration

	mu    sync.Mutex
	calls []string
}

func (f *fakeTools) Definitions() []llm.ToolDef { return f.defs }

func (f *fakeTools) Call(ctx context.Context, name string, _ map[string]any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err, failing := f.errs[name]; failing {
		return nil, err
	}
	if reply, known := f.replies[name]; known {
		return reply, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: "ok"}}}, nil
}

func (f *fakeTools) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: text}}}
}

// A question needing no tool must come straight back.
func TestAskAnswersWithoutTools(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{{Text: "Alan Turing was a mathematician."}}}
	agent := New(Config{Provider: provider, Tools: &fakeTools{}})

	answer, err := agent.Ask(context.Background(), "Who was Alan Turing?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer != "Alan Turing was a mathematician." {
		t.Errorf("answer = %q", answer)
	}
}

// The loop: the model asks for a tool, the result goes back, the model answers.
func TestAskRunsToolThenAnswers(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{
			Text: "Let me look.",
			ToolCalls: []llm.ToolCall{{
				ID:            "call-1",
				Name:          "filesystem__list_directory",
				Arguments:     map[string]any{"path": "/tmp"},
				ProviderState: []byte("signature"),
			}},
		},
		{Text: "There are two files."},
	}}
	tools := &fakeTools{replies: map[string]*mcp.CallToolResult{
		"filesystem__list_directory": textResult("[FILE] a\n[FILE] b"),
	}}

	agent := New(Config{Provider: provider, Tools: tools})
	answer, err := agent.Ask(context.Background(), "what is in /tmp?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer != "There are two files." {
		t.Errorf("answer = %q", answer)
	}

	history := agent.History()
	if len(history) != 4 {
		t.Fatalf("history has %d turns, want user, model, results, model", len(history))
	}

	// The model's turn must keep its tool calls, provider state included: they
	// are replayed verbatim and cannot be rebuilt from the text.
	modelTurn := history[1]
	if len(modelTurn.ToolCalls) != 1 {
		t.Fatalf("model turn lost its tool calls: %+v", modelTurn)
	}
	if string(modelTurn.ToolCalls[0].ProviderState) != "signature" {
		t.Error("provider state was dropped from the recorded tool call")
	}

	resultTurn := history[2]
	if len(resultTurn.ToolResults) != 1 {
		t.Fatalf("result turn = %+v", resultTurn)
	}
	if resultTurn.ToolResults[0].ID != "call-1" {
		t.Errorf("result lost the call id: %+v", resultTurn.ToolResults[0])
	}
	if resultTurn.ToolResults[0].Content != "[FILE] a\n[FILE] b" {
		t.Errorf("result content = %q", resultTurn.ToolResults[0].Content)
	}
}

// A tool that fails is the model's problem to solve, not a reason to end the
// conversation.
func TestFailingToolIsReportedToTheModel(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{Name: "filesystem__read_text_file"}}},
		{Text: "That file does not exist."},
	}}
	tools := &fakeTools{replies: map[string]*mcp.CallToolResult{
		"filesystem__read_text_file": {
			Content: []mcp.Content{{Type: "text", Text: "ENOENT"}},
			IsError: true,
		},
	}}

	agent := New(Config{Provider: provider, Tools: tools})
	answer, err := agent.Ask(context.Background(), "read /nope")
	if err != nil {
		t.Fatalf("Ask ended on a failing tool: %v", err)
	}
	if answer != "That file does not exist." {
		t.Errorf("answer = %q", answer)
	}

	result := agent.History()[2].ToolResults[0]
	if !result.IsError {
		t.Error("IsError was not carried through to the model")
	}
	if result.Content != "ENOENT" {
		t.Errorf("content = %q", result.Content)
	}
}

// A server that died is still reported as a tool failure. The model may reach
// the same goal through another server.
func TestTransportFailureIsReportedNotRaised(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{Name: "broken__do_thing"}}},
		{Text: "That server is unavailable."},
	}}
	tools := &fakeTools{errs: map[string]error{
		"broken__do_thing": errors.New("transport: closed"),
	}}

	agent := New(Config{Provider: provider, Tools: tools})
	if _, err := agent.Ask(context.Background(), "do the thing"); err != nil {
		t.Fatalf("a dead server ended the conversation: %v", err)
	}

	result := agent.History()[2].ToolResults[0]
	if !result.IsError {
		t.Error("a transport failure did not reach the model as a tool error")
	}
	if !strings.Contains(result.Content, "transport: closed") {
		t.Errorf("content = %q", result.Content)
	}
}

// Several tools in one turn run together, and their results must line up with
// the calls that produced them.
func TestParallelToolResultsKeepCallOrder(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "a", Name: "srv__slow"},
		{ID: "b", Name: "srv__fast"},
		{ID: "c", Name: "srv__middle"},
	}
	provider := &scriptedProvider{responses: []llm.Response{
		{ToolCalls: calls},
		{Text: "all done"},
	}}
	tools := &fakeTools{
		delay: 40 * time.Millisecond,
		replies: map[string]*mcp.CallToolResult{
			"srv__slow":   textResult("slow result"),
			"srv__fast":   textResult("fast result"),
			"srv__middle": textResult("middle result"),
		},
	}

	agent := New(Config{Provider: provider, Tools: tools})

	started := time.Now()
	if _, err := agent.Ask(context.Background(), "run three tools"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	elapsed := time.Since(started)

	// Run sequentially the three delays would exceed 120ms.
	if elapsed > 100*time.Millisecond {
		t.Errorf("three tools took %v; they did not run concurrently", elapsed)
	}

	results := agent.History()[2].ToolResults
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, call := range calls {
		if results[i].ID != call.ID || results[i].Name != call.Name {
			t.Fatalf("result %d is %+v, want the result of %+v", i, results[i], call)
		}
	}
	if results[0].Content != "slow result" {
		t.Errorf("results were reordered: %+v", results)
	}
}

// A model that never concludes would otherwise run until the budget is gone.
func TestAskStopsAtTheTurnLimit(t *testing.T) {
	looping := make([]llm.Response, 20)
	for i := range looping {
		looping[i] = llm.Response{ToolCalls: []llm.ToolCall{{Name: "srv__again"}}}
	}
	provider := &scriptedProvider{responses: looping}

	agent := New(Config{Provider: provider, Tools: &fakeTools{}, MaxTurns: 3})
	if _, err := agent.Ask(context.Background(), "loop forever"); err == nil {
		t.Fatal("Ask returned no error after exhausting its turns")
	}

	// The history must not end on a dangling tool result, or the next question
	// would follow one.
	history := agent.History()
	last := history[len(history)-1]
	if last.Role != llm.RoleModel {
		t.Errorf("history ends on a %s turn, want a closing model turn", last.Role)
	}
}

// Context carries the conversation, so the second question has to see the first.
func TestHistoryIsSentOnEveryTurn(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{Text: "He was a mathematician."},
		{Text: "He was born in 1912."},
	}}
	agent := New(Config{Provider: provider, Tools: &fakeTools{}})
	ctx := context.Background()

	if _, err := agent.Ask(ctx, "Who was Alan Turing?"); err != nil {
		t.Fatalf("first Ask: %v", err)
	}
	if _, err := agent.Ask(ctx, "When was he born?"); err != nil {
		t.Fatalf("second Ask: %v", err)
	}

	sent := provider.lastRequest(t)
	if len(sent.Messages) != 3 {
		t.Fatalf("second question carried %d messages, want the earlier turns too", len(sent.Messages))
	}
	if !strings.Contains(sent.Messages[0].Text, "Alan Turing") {
		t.Errorf("the first question was lost: %+v", sent.Messages[0])
	}
}

func TestToolDefinitionsAreSentToTheProvider(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{{Text: "hi"}}}
	tools := &fakeTools{defs: []llm.ToolDef{
		{Name: "filesystem__read_text_file"},
		{Name: "git__git_commit"},
	}}

	agent := New(Config{Provider: provider, Tools: tools})
	if _, err := agent.Ask(context.Background(), "hello"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	sent := provider.lastRequest(t)
	if len(sent.Tools) != 2 {
		t.Fatalf("provider received %d tools, want 2", len(sent.Tools))
	}
	if sent.System == "" {
		t.Error("no system prompt was sent")
	}
}

func TestObserverSeesToolProgress(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{Name: "srv__work"}}},
		{Text: "done"},
	}}

	var mu sync.Mutex
	var kinds []EventKind

	agent := New(Config{
		Provider: provider,
		Tools:    &fakeTools{},
		Observe: func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			kinds = append(kinds, e.Kind)
		},
	})
	if _, err := agent.Ask(context.Background(), "work"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[EventKind]bool{}
	for _, k := range kinds {
		seen[k] = true
	}
	for _, want := range []EventKind{ModelReplied, ToolStarted, ToolFinished} {
		if !seen[want] {
			t.Errorf("observer never saw %s; got %v", want, kinds)
		}
	}
}

func TestUsageAccumulatesAcrossTurns(t *testing.T) {
	provider := &scriptedProvider{responses: []llm.Response{
		{
			ToolCalls: []llm.ToolCall{{Name: "srv__work"}},
			Usage:     llm.Usage{InputTokens: 100, OutputTokens: 10},
		},
		{Text: "done", Usage: llm.Usage{InputTokens: 150, OutputTokens: 20}},
	}}

	agent := New(Config{Provider: provider, Tools: &fakeTools{}})
	if _, err := agent.Ask(context.Background(), "work"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	usage := agent.Usage()
	if usage.InputTokens != 250 || usage.OutputTokens != 30 {
		t.Errorf("Usage() = %+v, want 250 in and 30 out", usage)
	}
}

func TestProviderFailureIsReturned(t *testing.T) {
	provider := &scriptedProvider{err: fmt.Errorf("service unavailable")}
	agent := New(Config{Provider: provider, Tools: &fakeTools{}})

	if _, err := agent.Ask(context.Background(), "hello"); err == nil {
		t.Fatal("Ask hid a provider failure")
	}
}
