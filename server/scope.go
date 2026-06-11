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

// scopeFor decides which database namespace a request operates on.
//
// This is the core data-sharing policy of the platform: it controls
// whether sites can read and write each other's collections.
//
// TODO(sasha): pick the policy. The placeholder below isolates every
// site completely. Alternatives worth weighing:
//   - global namespace: any site can touch any collection (maximum
//     Geocities energy, enables mashups, zero data isolation)
//   - shared prefix: collections named "shared-*" resolve to a global
//     scope, everything else stays site-private (the ecosystem effect
//     from the blog post, with isolation by default)
func scopeFor(site, collection string) (string, error) {
	if site == "" {
		return "", fmt.Errorf("database API must be called from a site subdomain")
	}
	if !collectionRe.MatchString(collection) {
		return "", fmt.Errorf("invalid collection name %q: must match %s", collection, collectionRe)
	}
	return site, nil
}
