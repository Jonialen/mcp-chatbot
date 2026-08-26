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

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/host` | Entry point of the chatbot |
| `internal/jsonrpc` | JSON-RPC 2.0 envelope, client and request/response correlation |
| `internal/transport` | Frame transports: stdio for local servers, HTTP for remote ones |
| `internal/mcp` | MCP message types and session lifecycle |
| `internal/mcplog` | Human-readable log of every JSON-RPC frame |
| `config` | Declarative list of MCP servers to launch |

## License

Academic use.
