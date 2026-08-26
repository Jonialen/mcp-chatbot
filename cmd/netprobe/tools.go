package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/mcpserver"
)

// Bounds on every probe. A tool that waits indefinitely holds a request open on
// a server that is paying for the time.
const (
	probeTimeout = 10 * time.Second
	maxPortCount = 16
)

// register adds every tool this server publishes.
//
// The tools are deliberately about the network itself. This server is the one
// component of the project that is genuinely remote, so what it reports —
// resolution, connection, TLS negotiation, HTTP exchange — is the evidence for
// describing what happens at each layer beneath MCP.
func register(server *mcpserver.Server) {
	server.Register(mcpserver.Tool{
		Name:        "dns_lookup",
		Title:       "DNS Lookup",
		Description: "Resolve a hostname to its IPv4 and IPv6 addresses and report how long resolution took. Answers the question of what the name layer knows about a host.",
		InputSchema: schema(`{
			"type": "object",
			"properties": {
				"hostname": {"type": "string", "description": "The hostname to resolve, without a scheme or path."}
			},
			"required": ["hostname"]
		}`),
		Handler: dnsLookup,
	})

	server.Register(mcpserver.Tool{
		Name:        "tcp_probe",
		Title:       "TCP Probe",
		Description: "Open a TCP connection to one or more ports on a host and report which accepted, which refused, and how long the handshake took. Measures reachability at the transport layer.",
		InputSchema: schema(`{
			"type": "object",
			"properties": {
				"host":  {"type": "string", "description": "The host to connect to."},
				"ports": {
					"type": "array",
					"items": {"type": "integer", "minimum": 1, "maximum": 65535},
					"minItems": 1,
					"description": "The TCP ports to try."
				}
			},
			"required": ["host", "ports"]
		}`),
		Handler: tcpProbe,
	})

	server.Register(mcpserver.Tool{
		Name:        "http_probe",
		Title:       "HTTP Probe",
		Description: "Send a HEAD request to a URL and report the status, the response headers, the negotiated TLS version and cipher, and the time taken. Shows what the application and presentation layers agreed on.",
		InputSchema: schema(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "An http or https URL."}
			},
			"required": ["url"]
		}`),
		Handler: httpProbe,
	})
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func dnsLookup(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Hostname == "" {
		return "", fmt.Errorf("hostname is required")
	}

	lookupCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	started := time.Now()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, args.Hostname)
	elapsed := time.Since(started)
	if err != nil {
		return "", fmt.Errorf("resolving %s failed after %s: %w",
			args.Hostname, elapsed.Round(time.Millisecond), err)
	}

	var ipv4, ipv6 []string
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			ipv4 = append(ipv4, addr.IP.String())
		} else {
			ipv6 = append(ipv6, addr.IP.String())
		}
	}
	sort.Strings(ipv4)
	sort.Strings(ipv6)

	var out strings.Builder
	fmt.Fprintf(&out, "%s resolved in %s\n", args.Hostname, elapsed.Round(time.Millisecond))
	fmt.Fprintf(&out, "  IPv4: %s\n", join(ipv4))
	fmt.Fprintf(&out, "  IPv6: %s\n", join(ipv6))

	if cname, err := net.DefaultResolver.LookupCNAME(lookupCtx, args.Hostname); err == nil {
		if trimmed := strings.TrimSuffix(cname, "."); trimmed != args.Hostname {
			fmt.Fprintf(&out, "  CNAME: %s\n", trimmed)
		}
	}
	return out.String(), nil
}

func tcpProbe(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Host  string `json:"host"`
		Ports []int  `json:"ports"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Ports) == 0 {
		return "", fmt.Errorf("at least one port is required")
	}
	if len(args.Ports) > maxPortCount {
		return "", fmt.Errorf("at most %d ports may be probed at once, got %d",
			maxPortCount, len(args.Ports))
	}
	if err := checkTarget(ctx, args.Host); err != nil {
		return "", err
	}

	var out strings.Builder
	fmt.Fprintf(&out, "TCP probe of %s\n", args.Host)

	dialer := net.Dialer{}
	for _, port := range args.Ports {
		if port < 1 || port > 65535 {
			fmt.Fprintf(&out, "  %-6d invalid port\n", port)
			continue
		}

		dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		address := net.JoinHostPort(args.Host, fmt.Sprint(port))

		started := time.Now()
		conn, err := dialer.DialContext(dialCtx, "tcp", address)
		elapsed := time.Since(started)
		cancel()

		if err != nil {
			fmt.Fprintf(&out, "  %-6d closed  (%s, %v)\n", port,
				elapsed.Round(time.Millisecond), rootCause(err))
			continue
		}
		fmt.Fprintf(&out, "  %-6d open    (%s, local %s)\n", port,
			elapsed.Round(time.Millisecond), conn.LocalAddr())
		_ = conn.Close()
	}
	return out.String(), nil
}

func httpProbe(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	target, err := checkURL(ctx, args.URL)
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("cannot build request: %w", err)
	}
	req.Header.Set("User-Agent", "netprobe-mcp/1.0")

	client := &http.Client{
		// Redirects are reported rather than followed: where a URL sends a
		// client is part of what the probe is meant to reveal.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	started := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		return "", fmt.Errorf("request failed after %s: %w",
			elapsed.Round(time.Millisecond), rootCause(err))
	}
	defer resp.Body.Close()

	var out strings.Builder
	fmt.Fprintf(&out, "HEAD %s\n", target)
	fmt.Fprintf(&out, "  status:   %s (%s)\n", resp.Status, elapsed.Round(time.Millisecond))
	fmt.Fprintf(&out, "  protocol: %s\n", resp.Proto)

	if state := resp.TLS; state != nil {
		fmt.Fprintf(&out, "  tls:      %s, cipher %s\n",
			tlsVersion(state.Version), tls.CipherSuiteName(state.CipherSuite))
		if len(state.PeerCertificates) > 0 {
			cert := state.PeerCertificates[0]
			fmt.Fprintf(&out, "  cert:     %s, expires %s\n",
				cert.Subject.CommonName, cert.NotAfter.Format(time.DateOnly))
		}
	} else {
		fmt.Fprintf(&out, "  tls:      none (plaintext)\n")
	}

	if location := resp.Header.Get("Location"); location != "" {
		fmt.Fprintf(&out, "  redirect: %s\n", location)
	}

	fmt.Fprintf(&out, "  headers:\n")
	names := make([]string, 0, len(resp.Header))
	for name := range resp.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&out, "    %s: %s\n", name, strings.Join(resp.Header[name], ", "))
	}
	return out.String(), nil
}

func tlsVersion(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	default:
		return fmt.Sprintf("unknown (0x%04x)", version)
	}
}

// rootCause unwraps the layers net/http and net wrap an error in, so the
// message names what actually went wrong instead of restating the request.
func rootCause(err error) error {
	for {
		unwrapped := unwrap(err)
		if unwrapped == nil {
			return err
		}
		err = unwrapped
	}
}

func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}

func join(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}
