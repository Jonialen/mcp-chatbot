# MCP Chatbot Host

A console chatbot that acts as an **MCP host**: it coordinates several Model
Context Protocol clients, exposes their tools to a Claude model, and runs the
tool-use loop.

The Model Context Protocol is implemented **directly over JSON-RPC 2.0**, with
no MCP SDK. Every frame exchanged with a server is built, parsed and logged by
this codebase.

Course project for CC3067 Redes — Universidad del Valle de Guatemala.

## Status

Working end to end over local servers. The host connects to every server named
in its configuration, exposes their tools to the model as one list, and runs the
tool loop. Remote servers over HTTP are the next milestone.

See `docs/` for the assignment brief and the use case.

## Requirements

- Go 1.24 or newer
- Node.js (only to run the official MCP servers through `npx`)
- A Gemini API key in `GEMINI_API_KEY` (free from https://aistudio.google.com/apikey)

## Choice of model

The assignment suggests Anthropic because of its free credits, but the
requirement itself asks for "un LLM". This host uses Google Gemini, reached
through the `llm.Provider` port, and the MCP layer below it is unaware of that
choice.

Running Anthropic's own MCP servers against a Google model is the point: the
brief opens by observing that tool integrations are not portable between
vendors, and that MCP exists to make the tool independent of the model. Using
the vendor that designed the protocol would demonstrate none of that.

## Running

```sh
export GEMINI_API_KEY=...
mkdir -p workspace && git -C workspace init
go run ./cmd/host
```

| Flag | Meaning |
| --- | --- |
| `-config` | Server list to load (default `config/servers.json`) |
| `-logs` | Directory for the JSON-RPC frame log (default `logs`) |
| `-model` | Model to use |
| `-verbose` | Show whole JSON-RPC frames on screen |

In the session:

| Command | Meaning |
| --- | --- |
| `/tools [server]` | List the tools available |
| `/log` | Toggle whole frames on screen |
| `/usage` | Tokens spent in this conversation |
| `/reset` | Forget the conversation, keep the servers connected |
| `/quit` | Leave |

## Configuring servers

`config/servers.json` uses the same shape as Claude Desktop, so a server
published by somebody else can be added by pasting the block from its README.

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "./workspace"]
    },
    "git": {
      "command": "uvx",
      "args": ["mcp-server-git", "--repository", "./workspace"]
    }
  }
}
```

The label on the left becomes the prefix of that server's tools, which is how
two servers that both publish `read_file` stay apart. `"disabled": true` parks
an entry without connecting to it.

A server that fails to start is reported and stepped over; the rest of the
session continues without it.

## The frame log

Every run writes `logs/mcp-<timestamp>.log` holding every JSON-RPC frame, in
both directions, exactly as it crossed the transport. The screen shows one-line
summaries because a single `tools/list` result runs to thirteen kilobytes;
`/log` switches the screen to whole frames.

## Tests

```sh
go test ./... -race
```

Tests that call the real Gemini API are skipped unless `GEMINI_API_KEY` is set:

```sh
GEMINI_API_KEY=... go test ./internal/llm/gemini -run TestLive -v
```

They exist because unit tests can only prove what this code sends. Only the
service can prove what it accepts, and the schemas they use are captured from a
real run of the official filesystem server rather than written to be
convenient.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/host` | Entry point of the chatbot |
| `internal/jsonrpc` | JSON-RPC 2.0 envelope, client and request/response correlation |
| `internal/transport` | Frame transports: stdio for local servers, HTTP for remote ones |
| `internal/mcp` | MCP message types and session lifecycle |
| `internal/llm` | Provider port: the boundary between the chatbot and any model |
| `internal/llm/gemini` | Google Gemini adapter |
| `internal/mcplog` | Human-readable log of every JSON-RPC frame |
| `config` | Declarative list of MCP servers to launch |

## License

Academic use.
