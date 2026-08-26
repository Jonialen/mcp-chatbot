# MCP Chatbot Host

A console chatbot that acts as an **MCP host**: it coordinates several Model
Context Protocol clients, exposes their tools to a Claude model, and runs the
tool-use loop.

The Model Context Protocol is implemented **directly over JSON-RPC 2.0**, with
no MCP SDK. Every frame exchanged with a server is built, parsed and logged by
this codebase.

Course project for CC3067 Redes — Universidad del Valle de Guatemala.

## Status

Under development. The JSON-RPC layer, the stdio transport and the MCP session
lifecycle are implemented and exercised against the official filesystem server.
The model loop and the terminal interface are not built yet.

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

## Running the protocol smoke test

Launches the official filesystem MCP server, completes the handshake, lists its
tools and invokes one, printing every JSON-RPC frame in both directions.

```sh
# List the directories the server is allowed to reach
go run ./cmd/host -dir "$PWD"

# Invoke a tool that takes arguments
go run ./cmd/host -dir "$PWD" -tool list_directory -args '{"path":"'"$PWD"'"}'
```

| Flag | Meaning |
| --- | --- |
| `-dir` | Directory exposed through the filesystem server |
| `-tool` | Tool to invoke once the handshake completes |
| `-args` | Arguments for that tool, as a JSON object |

The filesystem server is an npm package and is launched through `npx`; Node only
has to be present, nothing else is installed by hand.

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
