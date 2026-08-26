// Package agent runs the conversation loop: it asks the model, executes the
// tools the model requests, feeds the results back, and repeats until the model
// answers the user instead of asking for another tool.
//
// The model never executes anything. It names a tool and its arguments; this
// package decides what that means, and the MCP layer carries it out. That split
// is the whole reason the protocol exists, and it is why a failing tool is
// reported to the model rather than raised: the model is the component that can
// choose a different approach.
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

// DefaultMaxTurns bounds one exchange.
//
// A model that keeps calling tools without concluding would otherwise run until
// the budget is gone. Each turn is a full request, so the ceiling is a cost
// ceiling as much as a safety one.
const DefaultMaxTurns = 12

// Tools is the set of tools available to the agent. *registry.Registry
// satisfies it.
type Tools interface {
	Definitions() []llm.ToolDef
	Call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
}

// EventKind identifies what happened.
type EventKind string

const (
	// ModelReplied is emitted after every model response, whether it answered
	// or asked for tools.
	ModelReplied EventKind = "model_replied"
	// ToolStarted is emitted before a tool runs.
	ToolStarted EventKind = "tool_started"
	// ToolFinished is emitted after a tool runs, successfully or not.
	ToolFinished EventKind = "tool_finished"
)

// Event reports progress so a terminal or a log can show what is happening
// while a turn is in flight.
type Event struct {
	Kind    EventKind
	Turn    int
	Tool    string
	Args    map[string]any
	IsError bool
	Elapsed time.Duration
	Usage   llm.Usage
}

// Observer receives events. It is called from the goroutine running the tool,
// so an implementation has to be safe for concurrent use.
type Observer func(Event)

// Agent holds one conversation.
type Agent struct {
	provider llm.Provider
	tools    Tools
	system   string
	maxTurns int
	observe  Observer

	mu      sync.Mutex
	history []llm.Message
	usage   llm.Usage
}

// Config configures an Agent.
type Config struct {
	Provider llm.Provider
	Tools    Tools

	// System frames the conversation. Empty means DefaultSystemPrompt.
	System string

	// MaxTurns bounds one exchange. Zero means DefaultMaxTurns.
	MaxTurns int

	// Observe, if set, receives progress events.
	Observe Observer
}

// DefaultSystemPrompt tells the model how to treat the tools it is given.
const DefaultSystemPrompt = `You are a helpful assistant with access to tools provided by MCP servers.

Tool names are prefixed with the server that owns them, as in filesystem__read_text_file.

Prefer a tool over your own recollection whenever one applies: the tools reach
real systems and your training data does not. If a tool reports an error, read
it and try a different approach rather than repeating the same call. When no
tool applies, answer directly.`

// New builds an Agent.
func New(cfg Config) *Agent {
	system := cfg.System
	if system == "" {
		system = DefaultSystemPrompt
	}

	maxTurns := cfg.MaxTurns
	if maxTurns < 1 {
		maxTurns = DefaultMaxTurns
	}

	return &Agent{
		provider: cfg.Provider,
		tools:    cfg.Tools,
		system:   system,
		maxTurns: maxTurns,
		observe:  cfg.Observe,
	}
}

// Ask sends the user's message and returns the model's answer, running as many
// rounds of tool calls as the model needs in between.
func (a *Agent) Ask(ctx context.Context, input string) (string, error) {
	a.append(llm.Message{Role: llm.RoleUser, Text: input})

	for turn := 1; turn <= a.maxTurns; turn++ {
		response, err := a.provider.Generate(ctx, llm.Request{
			System:   a.system,
			Messages: a.snapshot(),
			Tools:    a.tools.Definitions(),
		})
		if err != nil {
			return "", fmt.Errorf("agent: turn %d: %w", turn, err)
		}

		a.addUsage(response.Usage)

		// The model's turn is recorded whole, tool calls included. Those calls
		// carry provider state that has to be replayed verbatim, so the turn
		// cannot be reconstructed later from its text alone.
		a.append(llm.Message{
			Role:      llm.RoleModel,
			Text:      response.Text,
			ToolCalls: response.ToolCalls,
		})

		a.emit(Event{
			Kind:  ModelReplied,
			Turn:  turn,
			Usage: response.Usage,
		})

		if !response.WantsTools() {
			return response.Text, nil
		}

		results := a.runTools(ctx, turn, response.ToolCalls)
		a.append(llm.Message{Role: llm.RoleUser, ToolResults: results})
	}

	// Out of turns. A closing model turn is recorded so the history stays
	// well-formed and the next question does not follow a dangling tool result.
	note := fmt.Sprintf("Stopped after %d turns without reaching an answer.", a.maxTurns)
	a.append(llm.Message{Role: llm.RoleModel, Text: note})
	return "", fmt.Errorf("agent: %s", note)
}

// runTools executes one round of tool calls.
//
// A model may request several tools at once, and they are independent, so they
// run concurrently. The results are written back by index: providers pair a
// result with its call, and a reordered batch pairs them wrongly.
func (a *Agent) runTools(ctx context.Context, turn int, calls []llm.ToolCall) []llm.ToolResult {
	results := make([]llm.ToolResult, len(calls))

	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = a.runTool(ctx, turn, call)
		}()
	}
	wg.Wait()

	return results
}

// runTool executes one call and turns whatever happened into something the
// model can read.
//
// Every failure becomes a tool result with IsError set, including a server that
// crashed or a transport that died. Raising it instead would end the
// conversation over a problem the model may well be able to route around by
// using a different server.
func (a *Agent) runTool(ctx context.Context, turn int, call llm.ToolCall) llm.ToolResult {
	a.emit(Event{Kind: ToolStarted, Turn: turn, Tool: call.Name, Args: call.Arguments})

	started := time.Now()
	result, err := a.tools.Call(ctx, call.Name, call.Arguments)
	elapsed := time.Since(started)

	out := llm.ToolResult{ID: call.ID, Name: call.Name}
	switch {
	case err != nil:
		out.Content = err.Error()
		out.IsError = true
	case result == nil:
		out.Content = "the tool returned no result"
		out.IsError = true
	default:
		out.Content = result.Text()
		out.IsError = result.IsError
	}

	// A result has to carry something. An empty payload tells the model nothing
	// and some providers reject it outright.
	if out.Content == "" {
		out.Content = "(the tool produced no output)"
	}

	a.emit(Event{
		Kind:    ToolFinished,
		Turn:    turn,
		Tool:    call.Name,
		IsError: out.IsError,
		Elapsed: elapsed,
	})
	return out
}

// History returns a copy of the conversation so far.
func (a *Agent) History() []llm.Message { return a.snapshot() }

// Usage reports what this conversation has cost in tokens.
func (a *Agent) Usage() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// Reset clears the conversation, keeping the configuration.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = nil
	a.usage = llm.Usage{}
}

func (a *Agent) append(msg llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append(a.history, msg)
}

func (a *Agent) snapshot() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]llm.Message, len(a.history))
	copy(out, a.history)
	return out
}

func (a *Agent) addUsage(u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usage.InputTokens += u.InputTokens
	a.usage.OutputTokens += u.OutputTokens
}

func (a *Agent) emit(event Event) {
	if a.observe != nil {
		a.observe(event)
	}
}
