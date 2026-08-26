package gemini

import "encoding/json"

// metaKeywords are JSON Schema annotations that describe the schema document
// itself rather than constraining any value.
//
// MCP servers publish draft-07 schemas, so these travel along with every tool
// definition. They are dropped before the schema reaches Gemini because they
// carry no information the model can act on, and a validator that does not
// recognise them can reject the whole declaration over a field that never
// mattered.
var metaKeywords = map[string]bool{
	"$schema":  true,
	"$id":      true,
	"$comment": true,
}

// sanitizeSchema strips meta keywords from a JSON Schema, recursing through
// every nested schema.
//
// Nothing else is rewritten. Gemini accepts raw JSON Schema through
// ParametersJsonSchema, so translating draft-07 into the OpenAPI subset that
// the older Parameters field expects would lose constraints for no reason.
func sanitizeSchema(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		// A tool with no parameters still needs an object schema: a provider
		// that validates against "type": "object" rejects a missing one.
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return strip(decoded), nil
}

func strip(node any) any {
	switch value := node.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(value))
		for key, child := range value {
			if metaKeywords[key] {
				continue
			}
			cleaned[key] = strip(child)
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(value))
		for i, child := range value {
			cleaned[i] = strip(child)
		}
		return cleaned
	default:
		return node
	}
}
