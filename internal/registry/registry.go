// Package registry collects the tools of every connected MCP server into one
// list the model can choose from, and routes a chosen tool back to the server
// that owns it.
//
// Two problems make this more than a map. Servers are written independently and
// collide on obvious names — read_file, search, list — so every tool is
// qualified with the server it came from. And servers written by other people
// fail: one that will not start, or answers nothing, must not take the session
// down with it.
package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

// Separator joins a server's name to a tool's own name.
//
// Two underscores, because a single one is common inside tool names and would
// make the split ambiguous. The result stays inside what providers accept for a
// tool name: a letter or underscore first, then letters, digits, underscores,
// dots, colons or dashes.
const Separator = "__"

// MaxNameLength is the longest tool name providers accept.
const MaxNameLength = 128

// Server is one connected MCP server. *mcp.Session satisfies it.
type Server interface {
	Name() string
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
	Close() error
}

// entry is one tool and the server that owns it.
type entry struct {
	server Server
	local  string
	tool   mcp.Tool
}

// Registry aggregates the tools of several servers.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
	servers []Server
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

// Add lists a server's tools and registers them under qualified names.
//
// A server whose tools cannot be listed is reported and skipped, not fatal:
// with several servers connected, one bad neighbour must not cost the session
// every other tool.
func (r *Registry) Add(ctx context.Context, server Server) ([]string, error) {
	tools, err := server.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("registry: %s: %w", server.Name(), err)
	}

	prefix := sanitize(server.Name())

	r.mu.Lock()
	defer r.mu.Unlock()

	r.servers = append(r.servers, server)

	added := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := qualify(prefix, tool.Name)

		if existing, taken := r.entries[name]; taken {
			return added, fmt.Errorf(
				"registry: %q from %s collides with the same name from %s",
				name, server.Name(), existing.server.Name())
		}

		r.entries[name] = entry{server: server, local: tool.Name, tool: tool}
		added = append(added, name)
	}
	return added, nil
}

// Definitions returns every registered tool in the shape a provider expects,
// sorted by name so the list is identical between requests. A stable order is
// what lets a provider cache the prefix of a prompt that begins with the tools.
func (r *Registry) Definitions() []llm.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]llm.ToolDef, 0, len(r.entries))
	for name, e := range r.entries {
		defs = append(defs, llm.ToolDef{
			Name:        name,
			Description: e.tool.Description,
			InputSchema: e.tool.InputSchema,
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Len reports how many tools are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Has reports whether a qualified name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[name]
	return ok
}

// ServerOf returns the name of the server owning a qualified tool.
func (r *Registry) ServerOf(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return "", false
	}
	return e.server.Name(), true
}

// Call routes a qualified tool name to its server and strips the prefix before
// forwarding, because the server only knows its own local name.
//
// A tool that ran and failed comes back as a result with IsError set, not as an
// error: that failure belongs to the model, which can read it and try something
// else. An unknown name is returned the same way, so a model that invents a
// tool gets told so and can correct itself instead of ending the conversation.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	r.mu.RLock()
	e, known := r.entries[name]
	r.mu.RUnlock()

	if !known {
		return &mcp.CallToolResult{
			Content: []mcp.Content{{
				Type: "text",
				Text: fmt.Sprintf("no tool named %q is available", name),
			}},
			IsError: true,
		}, nil
	}
	return e.server.CallTool(ctx, e.local, args)
}

// Close shuts every registered server down, returning the first failure but
// always attempting all of them.
func (r *Registry) Close() error {
	r.mu.Lock()
	servers := r.servers
	r.servers = nil
	r.entries = make(map[string]entry)
	r.mu.Unlock()

	var firstErr error
	for _, server := range servers {
		if err := server.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// qualify builds the name the model sees.
//
// Providers cap a tool name at 128 characters. Rather than truncate the tool's
// own name, which is what the model reads to decide, the prefix gives way: the
// prefix exists for routing and the registry can still route on a shortened one
// because the whole qualified name is the key.
func qualify(prefix, tool string) string {
	name := prefix + Separator + tool
	if len(name) <= MaxNameLength {
		return name
	}

	room := MaxNameLength - len(Separator) - len(tool)
	if room < 1 {
		// Nothing left for a prefix: the tool name alone fills the budget.
		return truncate(sanitize(tool), MaxNameLength)
	}
	return prefix[:room] + Separator + tool
}

// sanitize turns a server label into something usable as part of a tool name.
//
// Server names come from a configuration file written by a person, so they may
// contain spaces, slashes or accents that providers reject.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	out := b.String()
	if out == "" {
		return "server"
	}
	// A name must start with a letter or an underscore, never a digit.
	if out[0] >= '0' && out[0] <= '9' {
		return "_" + out
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
