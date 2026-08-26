package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

// A tool that reaches the network on behalf of whoever calls it is a way into
// whatever the server can reach. On a cloud host that includes the metadata
// service and the rest of the private network.
func TestCheckIPRefusesInternalRanges(t *testing.T) {
	refused := []string{
		"127.0.0.1",       // loopback
		"::1",             // loopback, IPv6
		"10.0.0.5",        // private
		"192.168.1.1",     // private
		"172.16.0.1",      // private
		"169.254.169.254", // link-local: cloud metadata
		"0.0.0.0",         // unspecified
	}
	for _, addr := range refused {
		t.Run(addr, func(t *testing.T) {
			if err := checkIP(net.ParseIP(addr)); err == nil {
				t.Errorf("checkIP(%s) allowed an internal address", addr)
			}
		})
	}

	allowed := []string{"8.8.8.8", "140.82.112.4", "2001:4860:4860::8888"}
	for _, addr := range allowed {
		t.Run(addr, func(t *testing.T) {
			if err := checkIP(net.ParseIP(addr)); err != nil {
				t.Errorf("checkIP(%s) refused a public address: %v", addr, err)
			}
		})
	}
}

func TestCheckTargetRefusesLocalhostByName(t *testing.T) {
	for _, name := range []string{"localhost", "LOCALHOST", "LocalHost"} {
		if err := checkTarget(context.Background(), name); err == nil {
			t.Errorf("checkTarget(%q) was allowed", name)
		}
	}
}

func TestCheckTargetRejectsEmptyHost(t *testing.T) {
	if err := checkTarget(context.Background(), ""); err == nil {
		t.Error("checkTarget accepted an empty host")
	}
}

func TestCheckURLRequiresAnHTTPScheme(t *testing.T) {
	cases := []struct {
		url   string
		wants string
	}{
		{"file:///etc/passwd", "only http and https"},
		{"gopher://example.com", "only http and https"},
		{"ftp://example.com", "only http and https"},
		{"http://localhost:8080/", "refusing to probe"},
		{"http://127.0.0.1/", "refusing to probe"},
		{"://not a url", "cannot parse"},
	}

	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			_, err := checkURL(context.Background(), tc.url)
			if err == nil {
				t.Fatalf("checkURL(%q) was allowed", tc.url)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wants)
			}
		})
	}
}
