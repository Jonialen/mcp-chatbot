# MCP Chatbot Host

A console chatbot that acts as an **MCP host**: it coordinates several Model
Context Protocol clients, exposes their tools to a Claude model, and runs the
tool-use loop.

The Model Context Protocol is implemented **directly over JSON-RPC 2.0**, with
no MCP SDK. Every frame exchanged with a server is built, parsed and logged by
this codebase.

Course project for CC3067 Redes — Universidad del Valle de Guatemala.

## Status

Working end to end. The host connects to every server named in its
configuration — local ones over stdio, remote ones over Streamable HTTP —
exposes their tools to the model as one list, and runs the tool loop.

See `docs/` for the assignment brief and the use case.

## Servers in this project

Two MCP servers are written here, both speaking JSON-RPC directly:

| Server | Where it lives | Transport | What it does |
| --- | --- | --- | --- |
| **netprobe** | `cmd/netprobe` in this repository | HTTP | Resolves names, opens TCP connections and exchanges HTTP requests, reporting what each actually did. Built to run on a cloud host. |
| **BrewOps** | [Jonialen/brewops-mcp](https://github.com/Jonialen/brewops-mcp) | stdio | A speciality coffee shop's catalogue, recipes and roast profiles, with the arithmetic and reasoning that turn one into the other. |

BrewOps is a separate repository because it is published for other people to
run, and it is a self-contained module: a classmate builds it and gets one
static binary with no runtime to install.

### Running netprobe

In a container, which is how it is meant to run:

```sh
docker compose up -d --build
curl localhost:8080/health
```

Or directly, for development:

```sh
go run ./cmd/netprobe            # HTTP on $PORT, or :8080
go run ./cmd/netprobe -stdio     # as a local server over stdio
```

Then point the host at it:

```json
{ "mcpServers": { "netprobe": { "url": "http://localhost:8080/mcp" } } }
```

The image is built in two stages and ends at `distroless/static`, which carries
root certificates and nothing else — no shell, no package manager. The
certificates are the reason it is not built on `scratch`: `http_probe` opens TLS
connections, and an image without a certificate bundle fails every one of them
with an unverifiable-authority error that reads like a network fault.

The binary is linked statically (`CGO_ENABLED=0`) and the container runs as
`nonroot` with no capabilities and a read-only filesystem: a process that reaches
the network on behalf of whoever calls it should be able to do nothing else.

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
    },
    "brewops": {
      "command": "./bin/brewops",
      "args": ["-db", "./workspace/brewops.db"]
    },
    "netprobe": {
      "url": "https://example.invalid/mcp"
    }
  }
}
```

A server with a `command` is launched as a child process and spoken to over
stdio; one with a `url` is reached over Streamable HTTP. Nothing above the
transport layer knows which is which.

Those four servers are written in four different languages — TypeScript, Python
and two in Go — and the host adapts to none of them. That is the protocol's own
claim, and it is the point of the exercise.

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
| `internal/registry` | Aggregates every server's tools under namespaced names and routes calls back |
| `internal/agent` | The model-tool conversation loop |
| `internal/config` | The declarative server list |
| `internal/mcpserver` | The server half of MCP, used by `cmd/netprobe` |
| `cmd/netprobe` | A remote MCP server reporting on the network |
| `internal/mcplog` | Human-readable log of every JSON-RPC frame |
| `config` | Declarative list of MCP servers to launch |

## License

Academic use.
