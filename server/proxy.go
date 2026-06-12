package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// TrustedProxies identifies the front proxies whose X-Forwarded-* headers
// the API may trust. Direct callers can choose Host, but they must not be
// able to mint a different peer IP by sending their own forwarded headers.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

var defaultTrustedProxies = mustTrustedProxies("127.0.0.1/32,::1/128")

func NewTrustedProxies(raw string) (*TrustedProxies, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &TrustedProxies{}, nil
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("parse trusted proxy %q: %w", part, err)
			}
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("parse trusted proxy CIDR %q: %w", part, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return &TrustedProxies{prefixes: prefixes}, nil
}

func mustTrustedProxies(raw string) *TrustedProxies {
	trusted, err := NewTrustedProxies(raw)
	if err != nil {
		panic(err)
	}
	return trusted
}

func (t *TrustedProxies) ContainsRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range t.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
