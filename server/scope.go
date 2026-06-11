package main

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var collectionRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// siteFromHost extracts the site name from a request host. For host
// "mysite.quick.example.com" and quickDomain "quick.example.com" it
// returns "mysite". It returns "" for the apex domain, for hosts outside
// quickDomain, and for nested subdomains (a.b.quick.example.com).
func siteFromHost(host, quickDomain string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	quickDomain = strings.ToLower(quickDomain)
	if host == quickDomain {
		return ""
	}
	sub, found := strings.CutSuffix(host, "."+quickDomain)
	if !found || strings.Contains(sub, ".") || sub == "" {
		return ""
	}
	return sub
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
