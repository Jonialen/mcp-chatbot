package jsonrpc

import (
	"encoding/json"
	"testing"
)

// The three kinds of JSON-RPC frame share one wire shape, so classification is
// the only thing standing between a response and a request that must be
// answered. These cases pin that behaviour down.
func TestMessageClassification(t *testing.T) {
	cases := []struct {
		name           string
		frame          string
		isRequest      bool
		isResponse     bool
		isNotification bool
	}{
		{
			name:      "request has method and id",
			frame:     `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			isRequest: true,
		},
		{
			name:           "notification has method and no id",
			frame:          `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			isNotification: true,
		},
		{
			name:       "result response has id and no method",
			frame:      `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
			isResponse: true,
		},
		{
			name:       "error response has id and no method",
			frame:      `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`,
			isResponse: true,
		},
		{
			name:       "a null result is still a response",
			frame:      `{"jsonrpc":"2.0","id":7,"result":null}`,
			isResponse: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msg Message
			if err := json.Unmarshal([]byte(tc.frame), &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := msg.IsRequest(); got != tc.isRequest {
				t.Errorf("IsRequest() = %v, want %v", got, tc.isRequest)
			}
			if got := msg.IsResponse(); got != tc.isResponse {
				t.Errorf("IsResponse() = %v, want %v", got, tc.isResponse)
			}
			if got := msg.IsNotification(); got != tc.isNotification {
				t.Errorf("IsNotification() = %v, want %v", got, tc.isNotification)
			}
		})
	}
}

// A notification must go out without an id field at all. Sending "id":null
// would turn it into a malformed request that some servers try to answer.
func TestNotificationOmitsID(t *testing.T) {
	msg, err := NewNotification("notifications/initialized", nil)
	if err != nil {
		t.Fatalf("NewNotification: %v", err)
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["id"]; present {
		t.Errorf("notification carries an id field: %s", encoded)
	}
	if _, present := decoded["params"]; present {
		t.Errorf("nil params should be omitted, not sent as null: %s", encoded)
	}
}

func TestRequestCarriesVersionAndID(t *testing.T) {
	msg, err := NewRequest(42, "initialize", map[string]string{"protocolVersion": "2025-06-18"})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if msg.JSONRPC != Version {
		t.Errorf("JSONRPC = %q, want %q", msg.JSONRPC, Version)
	}
	if string(msg.ID) != "42" {
		t.Errorf("ID = %s, want 42", msg.ID)
	}
}
