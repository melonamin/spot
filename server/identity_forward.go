package main

import (
	"net/http"
	"strings"
)

// Default forward-auth header names follow the Remote-* convention used by
// Pangolin, Authelia, Authentik, and traefik-forward-auth. Pangolin emits a
// single Remote-Role; multi-group proxies emit Remote-Groups (repeated or
// comma-separated). Both shapes are handled by parseForwardGroups.
const (
	defaultForwardUserHeader   = "Remote-User"
	defaultForwardEmailHeader  = "Remote-Email"
	defaultForwardNameHeader   = "Remote-Name"
	defaultForwardGroupsHeader = "Remote-Groups"
)

// ForwardAuth resolves visitor identity from headers injected by a trusted
// authentication proxy (forward auth). It is the integration seam for an
// external identity provider that sits in front of Spot: the proxy
// authenticates the caller and asserts who they are via Remote-* headers,
// the same way a sandbox-deploy proxy would assert the human who owns the
// session.
//
// Trust is enforced by the caller, not here: Server.forwardAuthIdentity only
// consults a ForwardAuth when the request's socket peer is a trusted proxy,
// so an untrusted client cannot assert an identity by sending Remote-*
// headers. The proxy is responsible for stripping any client-supplied
// Remote-* headers before injecting its own.
type ForwardAuth struct {
	UserHeader   string
	EmailHeader  string
	NameHeader   string
	GroupsHeader string
}

// NewForwardAuth returns a ForwardAuth, falling back to the default Remote-*
// header name for any name left empty.
func NewForwardAuth(user, email, name, groups string) *ForwardAuth {
	return &ForwardAuth{
		UserHeader:   headerOr(user, defaultForwardUserHeader),
		EmailHeader:  headerOr(email, defaultForwardEmailHeader),
		NameHeader:   headerOr(name, defaultForwardNameHeader),
		GroupsHeader: headerOr(groups, defaultForwardGroupsHeader),
	}
}

func headerOr(name, fallback string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return fallback
}

// identityFrom builds an Identity from forward-auth headers. ok is false when
// no identifying header (email or user) is present, so the caller falls
// through to the mesh resolver. Email is lowercased to match actorKey, so the
// same user owns the same sites however they arrive (proxy or mesh).
func (f *ForwardAuth) identityFrom(h http.Header, ip string) (Identity, bool) {
	email := strings.ToLower(strings.TrimSpace(h.Get(f.EmailHeader)))
	user := strings.TrimSpace(h.Get(f.UserHeader))
	if email == "" && user == "" {
		return Identity{}, false
	}
	return Identity{
		Email:    email,
		Name:     strings.TrimSpace(h.Get(f.NameHeader)),
		PeerName: user,
		PeerIP:   ip,
		Groups:   parseForwardGroups(h.Values(f.GroupsHeader)),
	}, true
}

// parseForwardGroups flattens the group/role header into Spot group names.
// Proxies vary: some repeat the header, some send one comma-separated value,
// Pangolin sends a single role. Values are trimmed and de-duplicated and
// empty entries dropped. Group case is preserved because AccessPolicy.Allows
// matches case-insensitively. The result is non-nil so it serializes as [].
func parseForwardGroups(values []string) []string {
	seen := make(map[string]struct{})
	groups := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, dup := seen[part]; dup {
				continue
			}
			seen[part] = struct{}{}
			groups = append(groups, part)
		}
	}
	return groups
}
