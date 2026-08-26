package gemini

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
)

// A full agent round trip: the user asks, the model requests a tool, the host
// answers with the tool's output. Gemini pairs a result with its call, so the
// shape of that reply is not cosmetic.
func TestToContentsCarriesAToolRoundTrip(t *testing.T) {
	contents, err := toContents([]llm.Message{
		{Role: llm.RoleUser, Text: "list the project files"},
		{
			Role: llm.RoleModel,
			Text: "Let me look.",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-1",
				Name:      "filesystem__list_directory",
				Arguments: map[string]any{"path": "/tmp"},
			}},
		},
		{
			Role: llm.RoleUser,
			ToolResults: []llm.ToolResult{{
				ID:      "call-1",
				Name:    "filesystem__list_directory",
				Content: "[FILE] go.mod",
			}},
		},
	})
	if err != nil {
		t.Fatalf("toContents: %v", err)
	}
	if len(contents) != 3 {
		t.Fatalf("got %d contents, want 3", len(contents))
	}

	if contents[0].Role != string(genai.RoleUser) {
		t.Errorf("first turn role = %q", contents[0].Role)
	}

	// The model's turn carries its prose and its request together.
	modelTurn := contents[1]
	if modelTurn.Role != string(genai.RoleModel) {
		t.Errorf("model turn role = %q", modelTurn.Role)
	}
	if len(modelTurn.Parts) != 2 {
		t.Fatalf("model turn has %d parts, want text plus function call", len(modelTurn.Parts))
	}
	if modelTurn.Parts[0].Text != "Let me look." {
		t.Errorf("model text = %q", modelTurn.Parts[0].Text)
	}
	call := modelTurn.Parts[1].FunctionCall
	if call == nil {
		t.Fatal("model turn carries no function call")
	}
	if call.Name != "filesystem__list_directory" || call.ID != "call-1" {
		t.Errorf("function call = %+v", call)
	}
	if call.Args["path"] != "/tmp" {
		t.Errorf("args = %v", call.Args)
	}

	// The tool's output goes back as the user's turn: it is input to the model.
	resultTurn := contents[2]
	if resultTurn.Role != string(genai.RoleUser) {
		t.Errorf("tool result turn role = %q, want user", resultTurn.Role)
	}
	response := resultTurn.Parts[0].FunctionResponse
	if response == nil {
		t.Fatal("tool result turn carries no function response")
	}
	if response.ID != "call-1" || response.Name != "filesystem__list_directory" {
		t.Errorf("function response = %+v", response)
	}
	if response.Response[toolResultKey] != "[FILE] go.mod" {
		t.Errorf("response payload = %v", response.Response)
	}
}

// A tool that ran and failed reaches the model under a distinct key, so the
// model can tell a failure from output and pick another approach.
func TestFailedToolResultUsesErrorKey(t *testing.T) {
	contents, err := toContents([]llm.Message{{
		Role: llm.RoleUser,
		ToolResults: []llm.ToolResult{{
			Name:    "filesystem__read_text_file",
			Content: "ENOENT: no such file",
			IsError: true,
		}},
	}})
	if err != nil {
		t.Fatalf("toContents: %v", err)
	}

	payload := contents[0].Parts[0].FunctionResponse.Response
	if _, present := payload[toolErrorKey]; !present {
		t.Fatalf("failed tool reported under %v, want the error key", payload)
	}
	if _, present := payload[toolResultKey]; present {
		t.Error("a failed tool must not be reported as a result")
	}
}

// An empty turn would become a content with no parts, which the API rejects.
func TestToContentsSkipsEmptyTurns(t *testing.T) {
	contents, err := toContents([]llm.Message{
		{Role: llm.RoleUser, Text: "hello"},
		{Role: llm.RoleModel},
	})
	if err != nil {
		t.Fatalf("toContents: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want the empty turn dropped", len(contents))
	}
}

// The MCP input schema must reach the declaration through ParametersJsonSchema.
// Parameters expects the narrower OpenAPI subset and the two are mutually
// exclusive, so setting both is rejected outright.
func TestToToolUsesJSONSchemaFieldOnly(t *testing.T) {
	tool, err := toTool([]llm.ToolDef{{
		Name:        "filesystem__read_text_file",
		Description: "Read a file as text.",
		InputSchema: json.RawMessage(
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",` +
				`"properties":{"path":{"type":"string"}},"required":["path"]}`),
	}})
	if err != nil {
		t.Fatalf("toTool: %v", err)
	}
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("got %d declarations, want 1", len(tool.FunctionDeclarations))
	}

	decl := tool.FunctionDeclarations[0]
	if decl.Parameters != nil {
		t.Error("Parameters is set; it is mutually exclusive with ParametersJsonSchema")
	}
	if decl.ParametersJsonSchema == nil {
		t.Fatal("ParametersJsonSchema is nil")
	}

	schema := decl.ParametersJsonSchema.(map[string]any)
	if _, present := schema["$schema"]; present {
		t.Error("the declaration still carries $schema")
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v", schema["type"])
	}
}

// Namespaced tool names must satisfy Gemini's rule for a declaration name:
// start with a letter or underscore, then letters, digits, underscores, dots,
// colons or dashes, up to 128 characters.
func TestNamespacedToolNamesAreValidDeclarationNames(t *testing.T) {
	names := []string{
		"filesystem__read_text_file",
		"git__git_commit",
		"brewops__scale_recipe",
	}
	for _, name := range names {
		if len(name) > 128 {
			t.Errorf("%q exceeds 128 characters", name)
		}
		first := name[0]
		if !(first == '_' || (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
			t.Errorf("%q does not start with a letter or underscore", name)
		}
		for _, r := range name {
			valid := r == '_' || r == '.' || r == ':' || r == '-' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !valid {
				t.Errorf("%q contains the invalid character %q", name, r)
			}
		}
	}
}
