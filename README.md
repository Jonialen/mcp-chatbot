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

If port 8080 is taken on the host, move it without editing anything:

```sh
NETPROBE_PORT=8951 docker compose up -d --build
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
| `-tui` | Open the full-screen terminal UI (the default remains the line-based CLI) |

In the session:

| Command | Meaning |
| --- | --- |
| `/tools [server]` | List the tools available |
| `/log` | Toggle whole frames on screen |
| `/usage` | Tokens spent in this conversation |
| `/reset` | Forget the conversation, keep the servers connected |
| `/quit` | Leave |

### Terminal UI

From the repository root, with `GEMINI_API_KEY` exported and your configured
MCP servers ready, run:

```sh
go run ./cmd/host -tui
# Use a different server list or model with the same flags as the CLI:
go run ./cmd/host -tui -config config/servers.json
```

The host first opens a server selector using the entries in `-config`:

| Key | Selection action |
| --- | --- |
| Up / Down | Move through the server list |
| Space | Toggle the highlighted server |
| Enter | Connect the checked servers; an empty selection stays in the selector with a message |
| Esc / Ctrl+C | Cancel without starting servers, initializing Gemini, or creating a log |

Enabled entries start checked. Entries with `"disabled": true` start unchecked,
but you can explicitly select them for this session. Choices never rewrite the
configuration. To use only one server, uncheck the others. Restart with `-tui`
to choose a different set; there is no hot-switch command during chat.
Selection and cancellation do not require a Gemini key; connecting and chatting do.
Without `-tui`, the CLI still connects all non-disabled entries automatically.

After selection, the host connects and opens a full-screen chat showing the model,
token usage, connection results, tool activity, and replies. At least one tool
must be available, just as in CLI mode. The host does not load `.env` itself.
Use an interactive terminal (at least 30 columns by 10 rows; 80x24 recommended).

| Key / command | Action |
| --- | --- |
| Enter | Send the question or slash command |
| Left/Right, Home/End, Backspace | Edit the single-line prompt |
| Tab / Shift+Tab | Switch between Chat and the dedicated live Logs panel |
| PgUp / PgDn | Scroll the active panel (Up/Down also work in Logs) |
| Ctrl+Home / Ctrl+End | Jump to the beginning / latest output |
| `/tools [server]`, `/usage`, `/log`, `/help` | Use the existing host commands |
| `/reset` | Clear chat history, displayed transcript, and token counters; retain connections |
| Esc | Cancel the current request without exiting; wait for it to stop before sending again |
| `/quit`, `/exit`, Ctrl+C, Ctrl+D | Exit; Ctrl+C / Ctrl+D also cancel an in-flight request |

While a question is running, an animated indicator and elapsed time show that
the UI is updating, not that the provider has made progress. You can switch panels,
scroll, and draft the next prompt in Chat, but Enter is disabled to prevent
overlapping turns or a reset during tool execution. Logs include tool activity,
connection details, and provider retry/wait notices. A `* new` marker signals unseen
activity; Ctrl+End resumes following the latest output after scrolling up.
Cancellation cannot undo tool side effects already performed. Only use servers
and tools you trust: the TUI has the same execution permissions as the CLI.

Ctrl+C / Ctrl+D leave the alternate screen and restore the cursor before MCP
cleanup. Idle HTTP requests/streams are canceled on close; remote session deletion
gets at most three seconds per server. Local children receive EOF, then are killed
and reaped if they exceed the existing three-second shutdown grace. Esc only
requests cancellation of the current turn: it does not close the UI or disconnect
servers, and another turn stays disabled until the current one returns.

Each panel retains the latest 64,000 characters; this does not trim the agent's
conversation. JSON-RPC frames remain complete in the log file shown at startup.
`/log` changes only the on-screen detail and opens Logs; Tab returns to Chat.
Opening Logs with Tab does not change the logging detail. For redirected input or scripts, omit
`-tui` to keep the existing CLI behavior.

## Classmates' servers

Requirement 6 asks for two MCP servers written by other students. They are not
vendored here — they are their authors' repositories, cloned under `peers/`,
which is ignored by git.

| Server | Author | Language | Tools |
| --- | --- | --- | --- |
| `rrhh` | [NESHGP04/mcp-server-rrhh-construccion](https://github.com/NESHGP04/mcp-server-rrhh-construccion) | Python | 6 — HR management: employee lookup, vacation balances with carry-over, overtime pay by shift type, payroll and employment history |

To set it up:

```sh
git clone https://github.com/NESHGP04/mcp-server-rrhh-construccion.git peers/rrhh
cd peers/rrhh && python3.12 -m venv .venv && .venv/bin/pip install -r requirements.txt
```

Its entry is already in `config/servers.json`. Nothing in this host was changed
to accommodate it: it was added by pasting a block into the configuration, which
is the point of the format being declarative.

### Hotel server: install, configure, then select

[JosFer720/hotel-mcp-server](https://github.com/JosFer720/hotel-mcp-server)
is a **local stdio server**, not a hosted HTTP endpoint. A GitHub URL is source
code, not an MCP connection URL. It requires Python **3.11+**, has no runtime
dependencies, and starts with `python -m hotel_mcp`. The editable install uses
Hatchling as its build backend; Faker is optional and only needed to regenerate
sample data. The repository already includes `data/hotel.db`.

**Nothing from the hotel repository is installed or run by this change.** Review
the third-party code before installing it. From this host's repository root,
with Python 3.11+ available, these are manual setup commands:

```sh
mkdir -p peers
git clone https://github.com/JosFer720/hotel-mcp-server.git peers/hotel-mcp-server
python3 -m venv peers/hotel-mcp-server/.venv
peers/hotel-mcp-server/.venv/bin/python -m pip install -e ./peers/hotel-mcp-server
# First setup only: preserve the shipped database by using a working copy.
cp -n peers/hotel-mcp-server/data/hotel.db peers/hotel-mcp-server/data/hotel-session.db
```

Add this **entry inside the existing `mcpServers` object** in
`config/servers.json` (keep the other entries). Replace `/absolute/path/to` with
the full path to this host repository; JSON does not expand `$HOME` or `~`:

```json
"hotel": {
  "command": "/absolute/path/to/peers/hotel-mcp-server/.venv/bin/python",
  "args": ["-m", "hotel_mcp"],
  "cwd": "/absolute/path/to/peers/hotel-mcp-server",
  "env": [
    "HOTEL_DB=/absolute/path/to/peers/hotel-mcp-server/data/hotel-session.db",
    "HOTEL_EXPORT_DIR=/absolute/path/to/peers/hotel-mcp-server/comprobantes"
  ],
  "disabled": true
}
```

This host uses an **array of `KEY=VALUE` strings** for `env`, not the object in
some upstream examples. No `transport` field is needed. Absolute paths avoid
ambiguity when the child changes its working directory. The entry above is
documentation only: it has not been inserted into your active configuration.

Run `go run ./cmd/host -tui`. Move to `hotel`, press Space to check it, uncheck
other servers if you want hotel alone, then press Enter. With your Gemini key
already exported, the host launches hotel and opens chat. Use `/tools hotel`
to verify tool discovery, then try asking about availability for a specific date.
The host does not load `.env`; no `.env` change is needed for the selector.

**Use disposable data.** Hotel includes tools that update reservations and write
receipt files. This host currently executes model-requested tools without a
host-enforced human approval gate, including the cross-turn confirmation gate
recommended by hotel's README. Selecting a server is not per-tool approval or
a sandbox. This setup was checked against source, not by running hotel or Gemini.

Evidence at upstream revision `a0d82f6639c79a92f94549778a6c76912d698cf7`:
[README](https://github.com/JosFer720/hotel-mcp-server/blob/a0d82f6639c79a92f94549778a6c76912d698cf7/README.md),
[dependencies and entry point](https://github.com/JosFer720/hotel-mcp-server/blob/a0d82f6639c79a92f94549778a6c76912d698cf7/pyproject.toml),
[stdio startup](https://github.com/JosFer720/hotel-mcp-server/blob/a0d82f6639c79a92f94549778a6c76912d698cf7/src/hotel_mcp/__main__.py),
[database path](https://github.com/JosFer720/hotel-mcp-server/blob/a0d82f6639c79a92f94549778a6c76912d698cf7/src/hotel_mcp/db.py).

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
an entry without connecting to it by default; the TUI can opt it in for one session.

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
