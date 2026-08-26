package gemini

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The schemas MCP servers publish are draft-07 and carry a $schema annotation
// on every tool. It describes the schema document, constrains nothing, and is
// dropped before the declaration reaches the model.
func TestSanitizeStripsMetaKeywords(t *testing.T) {
	// Taken from the official filesystem server's read_text_file tool.
	raw := json.RawMessage(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"properties": {
			"path": {"type": "string"},
			"head": {"description": "first N lines", "type": "number"}
		},
		"required": ["path"]
	}`)

	got, err := sanitizeSchema(raw)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}

	schema, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("sanitizeSchema returned %T, want a map", got)
	}
	if _, present := schema["$schema"]; present {
		t.Error("$schema survived sanitisation")
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object", schema["type"])
	}
	if !reflect.DeepEqual(schema["required"], []any{"path"}) {
		t.Errorf("required = %v, want [path]", schema["required"])
	}
}

// Only meta keywords go. Every real constraint has to survive, or the model
// starts inventing arguments the server will reject.
func TestSanitizePreservesConstraints(t *testing.T) {
	raw := json.RawMessage(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"properties": {
			"paths": {"minItems": 1, "type": "array", "items": {"type": "string"}},
			"sortBy": {"default": "name", "type": "string", "enum": ["name", "size"]},
			"dryRun": {"default": false, "type": "boolean"},
			"kind": {"const": "resource"},
			"either": {"anyOf": [{"type": "string"}, {"type": "number"}]}
		},
		"required": ["paths"],
		"additionalProperties": false
	}`)

	got, err := sanitizeSchema(raw)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	schema := got.(map[string]any)

	if schema["additionalProperties"] != false {
		t.Error("additionalProperties was dropped")
	}

	props := schema["properties"].(map[string]any)

	paths := props["paths"].(map[string]any)
	if paths["minItems"] != float64(1) {
		t.Errorf("minItems = %v, want 1", paths["minItems"])
	}

	sortBy := props["sortBy"].(map[string]any)
	if sortBy["default"] != "name" {
		t.Errorf("default = %v, want name", sortBy["default"])
	}
	if !reflect.DeepEqual(sortBy["enum"], []any{"name", "size"}) {
		t.Errorf("enum = %v", sortBy["enum"])
	}

	if props["kind"].(map[string]any)["const"] != "resource" {
		t.Error("const was dropped")
	}
	if len(props["either"].(map[string]any)["anyOf"].([]any)) != 2 {
		t.Error("anyOf was dropped")
	}
}

// Nested schemas are sanitised too: a $schema hiding inside a property would
// otherwise reach the model untouched.
func TestSanitizeRecursesIntoNestedSchemas(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"edits": {
				"type": "array",
				"items": {
					"$schema": "http://json-schema.org/draft-07/schema#",
					"type": "object",
					"properties": {"oldText": {"$comment": "note", "type": "string"}}
				}
			}
		}
	}`)

	got, err := sanitizeSchema(raw)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}

	items := got.(map[string]any)["properties"].(map[string]any)["edits"].(map[string]any)["items"].(map[string]any)
	if _, present := items["$schema"]; present {
		t.Error("$schema survived inside an array item schema")
	}

	oldText := items["properties"].(map[string]any)["oldText"].(map[string]any)
	if _, present := oldText["$comment"]; present {
		t.Error("$comment survived inside a nested property")
	}
	if oldText["type"] != "string" {
		t.Error("nested type was lost")
	}
}

// A tool that takes no arguments still needs an object schema, for the same
// reason tools/call always sends an arguments object.
func TestSanitizeGivesEmptySchemaAnObjectShape(t *testing.T) {
	got, err := sanitizeSchema(nil)
	if err != nil {
		t.Fatalf("sanitizeSchema: %v", err)
	}
	schema := got.(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object", schema["type"])
	}
	if _, present := schema["properties"]; !present {
		t.Error("empty schema has no properties field")
	}
}

func TestSanitizeRejectsInvalidJSON(t *testing.T) {
	if _, err := sanitizeSchema(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("sanitizeSchema accepted malformed JSON")
	}
}
