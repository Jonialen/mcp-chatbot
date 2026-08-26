package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadReadsServers(t *testing.T) {
	path := write(t, `{
	  "mcpServers": {
	    "filesystem": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
	    },
	    "remote": {"url": "https://example.invalid/mcp"}
	  }
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(cfg.Servers))
	}

	fs := cfg.Servers["filesystem"]
	if fs.Command != "npx" || len(fs.Args) != 3 {
		t.Errorf("filesystem = %+v", fs)
	}
	if fs.IsRemote() {
		t.Error("a server with a command is not remote")
	}
	if !cfg.Servers["remote"].IsRemote() {
		t.Error("a server with a url is remote")
	}
}

// Registration order decides the order of the tool list, which is the prefix of
// every prompt. A map iterates randomly, so the order has to be imposed.
func TestEnabledIsStableAndSkipsDisabled(t *testing.T) {
	path := write(t, `{
	  "mcpServers": {
	    "zeta":  {"command": "z"},
	    "alpha": {"command": "a"},
	    "parked": {"command": "p", "disabled": true}
	  }
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	first := cfg.Enabled()
	if len(first) != 2 {
		t.Fatalf("got %d enabled servers, want the disabled one skipped", len(first))
	}
	if first[0].Name != "alpha" || first[1].Name != "zeta" {
		t.Fatalf("Enabled() = %v, want alphabetical", []string{first[0].Name, first[1].Name})
	}

	for range 20 {
		again := cfg.Enabled()
		for i := range first {
			if again[i].Name != first[i].Name {
				t.Fatal("Enabled() order changes between calls")
			}
		}
	}
}

func TestLoadRejectsInvalidConfigurations(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wants   string
	}{
		{"no servers", `{"mcpServers": {}}`, "no servers defined"},
		{"neither command nor url", `{"mcpServers": {"x": {}}}`, "neither a command nor a url"},
		{
			"both command and url",
			`{"mcpServers": {"x": {"command": "a", "url": "https://e.invalid"}}}`,
			"only be one",
		},
		{"unknown field", `{"mcpServers": {"x": {"commnad": "typo"}}}`, "unknown field"},
		{"malformed json", `{"mcpServers":`, "parse"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.content))
			if err == nil {
				t.Fatalf("Load accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// A file edited on Windows may carry a byte order mark, and the decoder's own
// error says nothing about it.
func TestLoadToleratesByteOrderMark(t *testing.T) {
	path := write(t, "\xEF\xBB\xBF"+`{"mcpServers": {"x": {"command": "a"}}}`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load rejected a file with a BOM: %v", err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("Load succeeded on a missing file")
	}
}
