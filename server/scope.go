package main

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var collectionRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// siteFromHost extracts the site name from a request host. For host
// "mysite.spot.example.com" and spotDomain "spot.example.com" it
// returns "mysite". It returns "" for the apex domain, for hosts outside
// spotDomain, and for nested subdomains (a.b.spot.example.com).
func siteFromHost(host, spotDomain string) string {
	host = cleanHost(host)
	spotDomain = strings.ToLower(strings.TrimSuffix(spotDomain, "."))
	if host == spotDomain {
		return ""
	}
	sub, found := strings.CutSuffix(host, "."+spotDomain)
	if !found || strings.Contains(sub, ".") || sub == "" || !siteNameRe.MatchString(sub) {
		return ""
	}
	return sub
}

func validSpotHost(host, spotDomain string) bool {
	if spotDomain == "" {
		return true
	}
	host = cleanHost(host)
	spotDomain = strings.ToLower(strings.TrimSuffix(spotDomain, "."))
	if host == "" || spotDomain == "" {
		return false
	}
	if host == spotDomain {
		return true
	}
	sub, found := strings.CutSuffix(host, "."+spotDomain)
	return found && !strings.Contains(sub, ".") && siteNameRe.MatchString(sub)
}

func cleanHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// sharedScope holds all "shared-*" collections. The underscore makes it
// unforgeable: site scopes come from hostname labels, which can never
// contain an underscore.
const sharedScope = "_shared"

// scopeFor decides which database namespace a request operates on.
//
// This is the core data-sharing policy of the platform: collections are
// private to their site, except those named "shared-*", which live in
// one global namespace that every site can read and write. The prefix
// makes sharing an explicit, visible choice in the collection name.
func scopeFor(site, collection string) (string, error) {
	if site == "" {
		return "", fmt.Errorf("database API must be called from a site subdomain")
	}
	if !collectionRe.MatchString(collection) {
		return "", fmt.Errorf("invalid collection name %q: must match %s", collection, collectionRe)
	}
	if strings.HasPrefix(collection, "shared-") {
		return sharedScope, nil
	}
	return site, nil
}

// roomScopeFor applies the same private-by-default, shared-* global
// namespace rule to ephemeral realtime rooms.
func roomScopeFor(site, room string) (string, error) {
	if site == "" {
		return "", fmt.Errorf("realtime rooms must be used from a site subdomain")
	}
	if !collectionRe.MatchString(room) {
		return "", fmt.Errorf("invalid room name %q: must match %s", room, collectionRe)
	}
	if strings.HasPrefix(room, "shared-") {
		return sharedScope, nil
	}
	return site, nil
}
