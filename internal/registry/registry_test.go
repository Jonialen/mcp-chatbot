package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

// fakeServer is a stand-in for a connected MCP server.
type fakeServer struct {
	name      string
	tools     []mcp.Tool
	listErr   error
	closed    bool
	lastCall  string
	lastArgs  map[string]any
	callErr   error
	callReply string
}

func (f *fakeServer) Name() string { return f.name }

func (f *fakeServer) ListTools(context.Context) ([]mcp.Tool, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

func (f *fakeServer) CallTool(_ context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	f.lastCall = name
	f.lastArgs = args
	if f.callErr != nil {
		return nil, f.callErr
	}
	return &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: f.callReply}}}, nil
}

func (f *fakeServer) Close() error {
	f.closed = true
	return nil
}

func tool(name string) mcp.Tool {
	return mcp.Tool{
		Name:        name,
		Description: "does " + name,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func mustAdd(t *testing.T, r *Registry, server Server) []string {
	t.Helper()
	names, err := r.Add(context.Background(), server)
	if err != nil {
		t.Fatalf("Add(%s): %v", server.Name(), err)
	}
	return names
}

// The reason the registry exists: independently written servers collide on
// obvious names, and both tools have to remain reachable.
func TestQualifiedNamesResolveCollisions(t *testing.T) {
	r := New()
	fs := &fakeServer{name: "filesystem", tools: []mcp.Tool{tool("read_file")}, callReply: "from fs"}
	git := &fakeServer{name: "git", tools: []mcp.Tool{tool("read_file")}, callReply: "from git"}

	mustAdd(t, r, fs)
	mustAdd(t, r, git)

	if r.Len() != 2 {
		t.Fatalf("Len() = %d, want both tools registered", r.Len())
	}
	if !r.Has("filesystem__read_file") || !r.Has("git__read_file") {
		t.Fatalf("qualified names missing: %v", r.Definitions())
	}

	ctx := context.Background()
	result, err := r.Call(ctx, "git__read_file", map[string]any{"path": "/x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Text() != "from git" {
		t.Errorf("call routed to the wrong server: %q", result.Text())
	}

	// The server must receive its own local name, not the qualified one.
	if git.lastCall != "read_file" {
		t.Errorf("server received %q, want the unqualified name", git.lastCall)
	}
	if fs.lastCall != "" {
		t.Error("the call reached the wrong server too")
	}
}

// A model that invents a tool has to be told so, in a form it can read and
// correct, rather than ending the conversation with an error.
func TestUnknownToolComesBackAsAToolError(t *testing.T) {
	r := New()
	mustAdd(t, r, &fakeServer{name: "filesystem", tools: []mcp.Tool{tool("read_file")}})

	result, err := r.Call(context.Background(), "filesystem__invented", nil)
	if err != nil {
		t.Fatalf("Call returned a Go error for an unknown tool: %v", err)
	}
	if !result.IsError {
		t.Error("IsError = false, want true")
	}
	if !strings.Contains(result.Text(), "invented") {
		t.Errorf("message does not name the missing tool: %q", result.Text())
	}
}

// A classmate's server that will not list its tools must not cost the session
// every other server's tools.
func TestFailingServerDoesNotAffectTheOthers(t *testing.T) {
	r := New()
	mustAdd(t, r, &fakeServer{name: "filesystem", tools: []mcp.Tool{tool("read_file")}})

	broken := &fakeServer{name: "broken", listErr: errors.New("server exited")}
	if _, err := r.Add(context.Background(), broken); err == nil {
		t.Fatal("Add succeeded for a server that cannot list its tools")
	}

	if r.Len() != 1 {
		t.Errorf("Len() = %d, want the healthy server's tool still registered", r.Len())
	}
	if !r.Has("filesystem__read_file") {
		t.Error("the healthy server's tool was lost")
	}
}

func TestDefinitionsAreSortedAndComplete(t *testing.T) {
	r := New()
	mustAdd(t, r, &fakeServer{name: "zeta", tools: []mcp.Tool{tool("b"), tool("a")}})
	mustAdd(t, r, &fakeServer{name: "alpha", tools: []mcp.Tool{tool("c")}})

	defs := r.Definitions()
	got := make([]string, len(defs))
	for i, d := range defs {
		got[i] = d.Name
	}

	want := []string{"alpha__c", "zeta__a", "zeta__b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Definitions() = %v, want %v", got, want)
		}
	}

	// A stable order is what lets a provider cache a prompt whose prefix is the
	// tool list, so the same registry must produce the same slice twice.
	second := r.Definitions()
	for i := range defs {
		if defs[i].Name != second[i].Name {
			t.Fatal("Definitions() is not stable between calls")
		}
	}

	if len(defs[0].InputSchema) == 0 {
		t.Error("the tool's schema was dropped on the way to the provider")
	}
}

// Names come from a configuration file written by a person and may contain
// anything; providers accept a narrow set.
func TestServerNamesAreSanitised(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"file system", "file_system"},
		{"café/tools", "caf__tools"},
		{"2brew", "_2brew"},
		{"", "server"},
		{"already-fine_1", "already-fine_1"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := sanitize(tc.label)
			if got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.label, got, tc.want)
			}
			assertValidToolName(t, got+Separator+"x")
		})
	}
}

// A long server name must not push a qualified name past the provider's limit,
// and must not eat into the tool's own name, which is what the model reads.
func TestQualifyStaysWithinTheNameLimit(t *testing.T) {
	longPrefix := strings.Repeat("s", 200)
	name := qualify(longPrefix, "read_text_file")

	if len(name) > MaxNameLength {
		t.Fatalf("qualified name is %d characters, over the %d limit", len(name), MaxNameLength)
	}
	if !strings.HasSuffix(name, Separator+"read_text_file") {
		t.Errorf("the tool's own name was truncated: %q", name)
	}
	assertValidToolName(t, name)
}

func TestRegistryClosesEveryServer(t *testing.T) {
	r := New()
	first := &fakeServer{name: "one", tools: []mcp.Tool{tool("a")}}
	second := &fakeServer{name: "two", tools: []mcp.Tool{tool("b")}}
	mustAdd(t, r, first)
	mustAdd(t, r, second)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !first.closed || !second.closed {
		t.Error("Close did not reach every server")
	}
	if r.Len() != 0 {
		t.Errorf("Len() = %d after Close, want 0", r.Len())
	}
}

// assertValidToolName checks the rule providers share: a letter or underscore
// first, then letters, digits, underscores, dots, colons or dashes, capped at
// 128 characters.
func assertValidToolName(t *testing.T, name string) {
	t.Helper()

	if name == "" || len(name) > MaxNameLength {
		t.Fatalf("invalid tool name length: %q (%d)", name, len(name))
	}
	first := name[0]
	if !(first == '_' || (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		t.Errorf("%q does not start with a letter or underscore", name)
	}
	for _, r := range name {
		ok := r == '_' || r == '.' || r == ':' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			t.Errorf("%q contains the invalid character %q", name, r)
		}
	}
}
