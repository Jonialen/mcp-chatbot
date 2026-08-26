// Package gemini adapts Google's Gemini models to the llm.Provider port.
//
// The assignment suggests Anthropic only because of its free credits, and the
// requirement itself asks for "un LLM". Running the official MCP servers
// against a Google model is a stronger demonstration of the protocol's own
// claim: that a tool is written once and consumed by any model.
package gemini

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
)

// DefaultModel is used when none is configured.
//
// A stable model, not a preview: preview endpoints answer 503 under load, which
// is not something a live demonstration should depend on.
const DefaultModel = "gemini-3.6-flash"

// DefaultFallbacks are tried, in order, once a model's free allowance for the
// day is gone.
//
// The free tier meters requests per model per day, so a second model is a
// second allowance. Without this the host stops working partway through an
// afternoon of development, and a demonstration that outlives its quota simply
// stops answering.
var DefaultFallbacks = []string{
	"gemini-3.5-flash",
	"gemini-3.5-flash-lite",
	"gemini-3.1-flash-lite",
}

// DefaultThinkingLevel keeps the chatbot responsive.
//
// Picking a tool out of a list and filling its arguments is a shallow decision,
// and the deeper levels cost tens of seconds per turn. Raise it through Config
// if a task turns out to need it.
const DefaultThinkingLevel = genai.ThinkingLevelLow

// toolResultKey is the field tool output is wrapped in. Gemini expects a
// function response to be a JSON object, so a plain string needs a home.
const toolResultKey = "result"

// toolErrorKey is where a failed tool's message goes, so the model can tell a
// failure from a result without parsing prose.
const toolErrorKey = "error"

// Provider talks to Gemini through the official Go SDK.
type Provider struct {
	client      *genai.Client
	thinking    genai.ThinkingLevel
	maxAttempts int
	onFallback  func(from, to string)

	// models is the fallback chain, most preferred first.
	models []string

	// current indexes models. It only ever moves forward: a model whose daily
	// allowance ran out will not recover within this session.
	mu      sync.Mutex
	current int
}

// Config configures a Provider.
type Config struct {
	// APIKey authenticates against the Gemini Developer API. When empty it is
	// read from GEMINI_API_KEY.
	APIKey string

	// Model selects the model. Empty means DefaultModel.
	Model string

	// ThinkingLevel controls how much the model reasons before answering.
	// Empty means DefaultThinkingLevel.
	ThinkingLevel genai.ThinkingLevel

	// MaxAttempts bounds retries of an overloaded or rate-limited service.
	// Zero means defaultMaxAttempts; one disables retrying.
	MaxAttempts int

	// Fallbacks are tried in order once a model's daily allowance is gone.
	// Nil means DefaultFallbacks; an empty slice disables falling back.
	Fallbacks []string

	// OnFallback, if set, is told when the provider moves to another model.
	// Switching silently would leave a session answering from a model nobody
	// chose, with no sign of why the answers changed.
	OnFallback func(from, to string)
}

// New builds a Provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini: no API key: set GEMINI_API_KEY")
	}

	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}

	thinking := cfg.ThinkingLevel
	if thinking == "" {
		thinking = DefaultThinkingLevel
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}
	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = defaultMaxAttempts
	}

	fallbacks := cfg.Fallbacks
	if fallbacks == nil {
		fallbacks = DefaultFallbacks
	}

	return &Provider{
		client:      client,
		thinking:    thinking,
		maxAttempts: attempts,
		onFallback:  cfg.OnFallback,
		models:      chain(model, fallbacks),
	}, nil
}

// chain builds the ordered list of models to try, without repeating one.
func chain(primary string, fallbacks []string) []string {
	models := []string{primary}
	for _, model := range fallbacks {
		if model != "" && model != primary {
			models = append(models, model)
		}
	}
	return models
}

func (p *Provider) Name() string { return "google" }

// Model returns the model currently in use, which is not necessarily the one
// configured: it advances when a daily allowance runs out.
func (p *Provider) Model() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.models[p.current]
}

// nextModel moves to the next model in the chain, reporting whether there was
// one. Comparing against the model that failed keeps two callers racing on the
// same exhausted model from skipping a healthy one between them.
func (p *Provider) nextModel(failed string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.models[p.current] != failed {
		// Another call already moved on; use where it landed.
		return p.models[p.current], true
	}
	if p.current+1 >= len(p.models) {
		return "", false
	}
	p.current++
	return p.models[p.current], true
}

// Generate sends one request and returns the model's reply.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	contents, err := toContents(req.Messages)
	if err != nil {
		return nil, err
	}

	config := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: p.thinking},
	}
	if req.System != "" {
		config.SystemInstruction = genai.NewContentFromText(req.System, genai.RoleUser)
	}
	if req.MaxOutputTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxOutputTokens)
	}
	if len(req.Tools) > 0 {
		tool, err := toTool(req.Tools)
		if err != nil {
			return nil, err
		}
		config.Tools = []*genai.Tool{tool}
	}

	// Each model in the chain gets its own retry budget: an overloaded service
	// and an exhausted allowance are different failures with different cures.
	for {
		model := p.Model()

		result, err := withRetry(ctx, p.maxAttempts, func() (*genai.GenerateContentResponse, error) {
			return p.client.Models.GenerateContent(ctx, model, contents, config)
		})
		if err == nil {
			return fromResult(result), nil
		}

		if !quotaExhausted(err) {
			return nil, fmt.Errorf("gemini: generate with %s: %w", model, err)
		}

		next, available := p.nextModel(model)
		if !available {
			return nil, fmt.Errorf(
				"gemini: the free allowance of every configured model is spent (%s): %w",
				strings.Join(p.models, ", "), err)
		}
		if p.onFallback != nil {
			p.onFallback(model, next)
		}
	}
}

// toTool turns the host's tool definitions into a single Gemini tool holding
// every declaration.
func toTool(tools []llm.ToolDef) (*genai.Tool, error) {
	declarations := make([]*genai.FunctionDeclaration, 0, len(tools))

	for _, tool := range tools {
		// ParametersJsonSchema takes JSON Schema directly, so an MCP server's
		// published schema reaches the model essentially as it was written.
		// It is mutually exclusive with Parameters, which expects the narrower
		// OpenAPI subset; setting both is rejected.
		schema, err := sanitizeSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("gemini: schema of tool %q: %w", tool.Name, err)
		}

		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:                 tool.Name,
			Description:          tool.Description,
			ParametersJsonSchema: schema,
		})
	}
	return &genai.Tool{FunctionDeclarations: declarations}, nil
}

// toContents replays the conversation in the shape Gemini expects.
//
// Tool results are not a role of their own here: they travel inside a user turn
// as function-response parts, which is how Gemini pairs a result with the call
// that produced it.
func toContents(messages []llm.Message) ([]*genai.Content, error) {
	contents := make([]*genai.Content, 0, len(messages))

	for _, msg := range messages {
		parts, err := toParts(msg)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, &genai.Content{
			Parts: parts,
			Role:  string(roleOf(msg)),
		})
	}
	return contents, nil
}

func toParts(msg llm.Message) ([]*genai.Part, error) {
	var parts []*genai.Part

	if msg.Text != "" {
		parts = append(parts, &genai.Part{Text: msg.Text})
	}

	for _, call := range msg.ToolCalls {
		// The signature the model issued with this call travels back on the
		// same part. Gemini 3 models reject a replayed tool call without it:
		// "Function call is missing a thought_signature in functionCall parts."
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   call.ID,
				Name: call.Name,
				Args: call.Arguments,
			},
			ThoughtSignature: call.ProviderState,
		})
	}

	for _, result := range msg.ToolResults {
		// A failed tool is reported under a distinct key rather than as a
		// transport error, so the model can recognise the failure and choose a
		// different approach instead of the loop aborting on its behalf.
		key := toolResultKey
		if result.IsError {
			key = toolErrorKey
		}
		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				ID:       result.ID,
				Name:     result.Name,
				Response: map[string]any{key: result.Content},
			},
		})
	}
	return parts, nil
}

// roleOf maps a turn onto a Gemini role. Tool results are sent as the user's
// turn because they are input to the model, not output from it.
func roleOf(msg llm.Message) genai.Role {
	if msg.Role == llm.RoleModel {
		return genai.RoleModel
	}
	return genai.RoleUser
}

// fromResult reads the model's reply.
//
// The parts are walked by hand rather than through the SDK's Text and
// FunctionCalls helpers. FunctionCalls returns the calls detached from the
// parts that carried them, which loses the signature each call has to be
// replayed with, and Text does not distinguish the model's reasoning from its
// answer.
func fromResult(result *genai.GenerateContentResponse) *llm.Response {
	response := &llm.Response{}

	if len(result.Candidates) > 0 && result.Candidates[0].Content != nil {
		var text strings.Builder

		for _, part := range result.Candidates[0].Content.Parts {
			switch {
			case part == nil:
			case part.Thought:
				// The model's reasoning, not its answer to the user.
			case part.FunctionCall != nil:
				response.ToolCalls = append(response.ToolCalls, llm.ToolCall{
					ID:            part.FunctionCall.ID,
					Name:          part.FunctionCall.Name,
					Arguments:     part.FunctionCall.Args,
					ProviderState: part.ThoughtSignature,
				})
			case part.Text != "":
				text.WriteString(part.Text)
			}
		}
		response.Text = text.String()
	}

	if usage := result.UsageMetadata; usage != nil {
		response.Usage = llm.Usage{
			InputTokens:  int(usage.PromptTokenCount),
			OutputTokens: int(usage.CandidatesTokenCount),
		}
	}
	return response
}

// compile-time proof that the adapter satisfies the port.
var _ llm.Provider = (*Provider)(nil)
