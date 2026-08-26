package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
)

// requestBodyLimit caps an incoming frame. A tool call is small; anything much
// larger is a mistake or an attack, and reading it costs memory either way.
const requestBodyLimit = 4 << 20

// sessionTTL is how long an idle session is remembered.
const sessionTTL = 30 * time.Minute

// HTTPHandler serves this server over MCP's Streamable HTTP transport.
//
// One endpoint carries everything: a client POSTs a frame and receives the
// answer in the same response. This handler answers in JSON rather than as an
// event stream, which the transport allows, because none of these tools streams
// partial results.
type HTTPHandler struct {
	server   *Server
	sessions *sessionStore
}

// NewHTTPHandler wraps a Server for use with net/http.
func NewHTTPHandler(server *Server) *HTTPHandler {
	return &HTTPHandler{server: server, sessions: newSessionStore()}
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)

	case http.MethodDelete:
		// The client is ending its session explicitly.
		h.sessions.drop(r.Header.Get("Mcp-Session-Id"))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		// A client may open a stream for server-initiated messages. This
		// server never sends any, so it declines rather than holding a
		// connection open forever for nothing.
		http.Error(w, "this server does not push messages", http.StatusMethodNotAllowed)

	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (h *HTTPHandler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, requestBodyLimit))
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}

	var msg jsonrpc.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "malformed JSON-RPC frame", http.StatusBadRequest)
		return
	}

	// A session id is minted on initialize and echoed by the client from then
	// on, which is how a stateful server keeps several clients apart.
	if msg.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", h.sessions.create())
	}

	response := h.server.Handle(r.Context(), &msg)
	if response == nil {
		// A notification has no reply. 202 says it was accepted and that the
		// empty body is deliberate.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// The status is already sent; there is nothing left but to stop.
		return
	}
}

// sessionStore remembers the sessions this server has issued.
//
// The tools here are stateless, so a session carries no data. It exists because
// the transport defines one, and because a server that hands out ids it does
// not track cannot expire them.
type sessionStore struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{lastSeen: make(map[string]time.Time)}
}

func (s *sessionStore) create() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice; a timestamp keeps the server
		// answering rather than refusing the handshake if it ever does.
		return "session-" + time.Now().UTC().Format("20060102150405.000000")
	}
	id := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.expire()
	s.lastSeen[id] = time.Now()
	return id
}

func (s *sessionStore) drop(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastSeen, id)
}

// expire removes sessions nobody has touched. The caller holds the lock.
func (s *sessionStore) expire() {
	cutoff := time.Now().Add(-sessionTTL)
	for id, seen := range s.lastSeen {
		if seen.Before(cutoff) {
			delete(s.lastSeen, id)
		}
	}
}
