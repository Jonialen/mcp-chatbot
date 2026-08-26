package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// resolveTimeout bounds a name lookup made while checking a target.
const resolveTimeout = 5 * time.Second

// checkTarget refuses hosts this server must not be pointed at.
//
// A tool that performs network requests on behalf of whoever calls it is a way
// into whatever the server itself can reach. Deployed on a cloud host, that
// includes the provider's metadata service and anything else inside the private
// network, so a target is resolved first and rejected if any of its addresses
// is loopback, private, link-local or unspecified.
func checkTarget(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("no host given")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("refusing to probe %q", host)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%q resolved to no addresses", host)
	}

	for _, addr := range addrs {
		if err := checkIP(addr.IP); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP) error {
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("refusing to probe the loopback address %s", ip)
	case ip.IsPrivate():
		return fmt.Errorf("refusing to probe the private address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// This range holds cloud metadata services.
		return fmt.Errorf("refusing to probe the link-local address %s", ip)
	case ip.IsUnspecified():
		return fmt.Errorf("refusing to probe the unspecified address %s", ip)
	default:
		return nil
	}
}

// checkURL validates a URL and the host inside it.
func checkURL(ctx context.Context, raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("only http and https are supported, got %q", parsed.Scheme)
	}
	if err := checkTarget(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}
