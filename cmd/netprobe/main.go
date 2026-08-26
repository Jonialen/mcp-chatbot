// Command netprobe is a remote MCP server that reports on the network.
//
// It publishes tools that resolve names, open TCP connections and exchange HTTP
// requests, and it reports what each of those actually did: the addresses a name
// carries, the time a handshake took, the TLS version and cipher two endpoints
// settled on. Being the one genuinely remote component of this project, what it
// observes is evidence about the layers underneath the protocol rather than a
// description of them.
//
// It speaks MCP over Streamable HTTP and is meant to run on a cloud host. The
// same binary serves stdio, so it can be attached to a host locally while it is
// being developed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/mcpserver"
)

const (
	serverName    = "netprobe"
	serverVersion = "1.0.0"

	// readHeaderTimeout guards against a client that opens a connection and
	// sends its headers slowly, holding a worker for as long as it likes.
	readHeaderTimeout = 10 * time.Second

	// shutdownGrace lets requests in flight finish when the host asks the
	// process to stop, which a cloud runtime does on every revision.
	shutdownGrace = 15 * time.Second
)

func main() {
	stdio := flag.Bool("stdio", false, "serve over stdio instead of HTTP")
	addr := flag.String("addr", "", "address to listen on (default :$PORT, or :8080)")
	flag.Parse()

	// Logs go to stderr. On stdio, stdout carries protocol frames and nothing
	// else: one stray line there corrupts the stream for the client.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.LUTC)

	server := mcpserver.New(serverName, serverVersion)
	register(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if *stdio {
		err = serveStdio(ctx, server)
	} else {
		err = serveHTTP(ctx, server, listenAddress(*addr))
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("%s: %v", serverName, err)
	}
}

func serveStdio(ctx context.Context, server *mcpserver.Server) error {
	log.Printf("%s %s serving on stdio", serverName, serverVersion)
	return server.ServeStdio(ctx, os.Stdin, os.Stdout)
}

func serveHTTP(ctx context.Context, server *mcpserver.Server, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.NewHTTPHandler(server))

	// A cloud runtime needs a cheap endpoint to decide the instance is alive,
	// and it will not speak MCP to find out.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","server":%q,"version":%q}`, serverName, serverVersion)
	})

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	listening := make(chan error, 1)
	go func() {
		log.Printf("%s %s listening on %s (endpoint /mcp)", serverName, serverVersion, addr)
		listening <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-listening:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err

	case <-ctx.Done():
		log.Printf("%s: shutting down", serverName)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// listenAddress honours the port a cloud runtime assigns.
//
// Cloud Run and similar hosts pass the port in an environment variable and
// route to whatever the container binds, so hard-coding one is what makes a
// deployment fail with nothing in the logs.
func listenAddress(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}
