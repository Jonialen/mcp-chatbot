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

	"google.golang.org/genai"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
)

// DefaultModel is used when none is configured.
const DefaultModel = "gemini-3-flash-preview"

// toolResultKey is the field tool output is wrapped in. Gemini expects a
// function response to be a JSON object, so a plain string needs a home.
const toolResultKey = "result"

// toolErrorKey is where a failed tool's message goes, so the model can tell a
// failure from a result without parsing prose.
const toolErrorKey = "error"

// Provider talks to Gemini through the official Go SDK.
type Provider struct {
	client *genai.Client
	model  string
}

// Config configures a Provider.
type Config struct {
	// APIKey authenticates against the Gemini Developer API. When empty it is
	// read from GEMINI_API_KEY.
	APIKey string

	// Model selects the model. Empty means DefaultModel.
	Model string
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

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}
	return &Provider{client: client, model: model}, nil
}

func (p *Provider) Name() string  { return "google" }
func (p *Provider) Model() string { return p.model }

// Generate sends one request and returns the model's reply.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	contents, err := toContents(req.Messages)
	if err != nil {
		return nil, err
	}

	config := &genai.GenerateContentConfig{}
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

	result, err := p.client.Models.GenerateContent(ctx, p.model, contents, config)
	if err != nil {
		return nil, fmt.Errorf("gemini: generate: %w", err)
	}
	return fromResult(result), nil
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
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   call.ID,
				Name: call.Name,
				Args: call.Arguments,
			},
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

func fromResult(result *genai.GenerateContentResponse) *llm.Response {
	response := &llm.Response{Text: result.Text()}

	for _, call := range result.FunctionCalls() {
		response.ToolCalls = append(response.ToolCalls, llm.ToolCall{
			ID:        call.ID,
			Name:      call.Name,
			Arguments: call.Args,
		})
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
