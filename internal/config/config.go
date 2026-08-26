// Package config reads the declarative list of MCP servers the host connects
// to.
//
// The format mirrors the one Claude Desktop uses, so a server published by
// somebody else can be added by pasting the block from its README instead of
// editing this program.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// DefaultStartupTimeout bounds one server's handshake.
//
// Launchers such as npx and uvx may download a package on a cold cache, so the
// first start of a server is slow in a way later ones are not.
const DefaultStartupTimeout = 90 * time.Second

// Config is the whole file.
type Config struct {
	// Servers maps a label to the server launched under it. The label is the
	// prefix every one of that server's tools receives.
	Servers map[string]Server `json:"mcpServers"`
}

// Server describes how to launch or reach one MCP server.
type Server struct {
	// Command and Args launch a local server over stdio.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Env holds extra KEY=VALUE entries for the child process.
	Env []string `json:"env,omitempty"`

	// Dir is the child process's working directory.
	Dir string `json:"cwd,omitempty"`

	// URL reaches a remote server over HTTP instead of launching a process.
	URL string `json:"url,omitempty"`

	// Disabled keeps an entry in the file without connecting to it, which is
	// how a classmate's broken server is parked without losing its definition.
	Disabled bool `json:"disabled,omitempty"`
}

// IsRemote reports whether this server is reached over HTTP.
func (s Server) IsRemote() bool { return s.URL != "" }

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	decoder := json.NewDecoder(newTrimReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// Enabled returns the servers to connect to, in a stable order so that tool
// registration, and therefore the prompt sent to the model, does not change
// between runs.
func (c *Config) Enabled() []NamedServer {
	names := make([]string, 0, len(c.Servers))
	for name, server := range c.Servers {
		if !server.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]NamedServer, 0, len(names))
	for _, name := range names {
		out = append(out, NamedServer{Name: name, Server: c.Servers[name]})
	}
	return out
}

// NamedServer pairs a server with its label.
type NamedServer struct {
	Name string
	Server
}

func (c *Config) validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("no servers defined under mcpServers")
	}

	for name, server := range c.Servers {
		if name == "" {
			return fmt.Errorf("a server has an empty name")
		}
		switch {
		case server.Command == "" && server.URL == "":
			return fmt.Errorf("server %q has neither a command nor a url", name)
		case server.Command != "" && server.URL != "":
			return fmt.Errorf("server %q has both a command and a url; it can only be one", name)
		}
	}
	return nil
}
