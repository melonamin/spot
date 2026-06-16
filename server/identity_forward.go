package main

import (
	"crypto/subtle"
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

// forwardAuthSecretHeader carries the shared secret a forward-auth proxy
// presents to prove itself when SPOT_FORWARD_AUTH_SECRET is set. This lets the
// proxy run off-mesh, where its source IP is not a reliable identifier.
const forwardAuthSecretHeader = "X-Spot-Forward-Auth-Secret"

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
	// Secret, when non-empty, is required in forwardAuthSecretHeader and
	// replaces the source-IP trust check (see authorized).
	Secret string
}

// authorized reports whether the request proves it comes from the trusted
// proxy. With a shared secret configured, the request must present it in the
// X-Spot-Forward-Auth-Secret header (constant-time compared), and the source
// IP is not required — so the proxy can run off-mesh. Without a secret, trust
// falls back to the source IP being in SPOT_TRUSTED_PROXIES.
func (f *ForwardAuth) authorized(r *http.Request, trustedIP bool) bool {
	if f.Secret == "" {
		return trustedIP
	}
	got := r.Header.Get(forwardAuthSecretHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(f.Secret)) == 1
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
// no email is present, so the caller falls through to the mesh resolver. Email
// is required because Spot keys ownership by email before peer IP; proxy user
// IDs are descriptive, not stable authorization keys.
func (f *ForwardAuth) identityFrom(h http.Header, ip string) (Identity, bool) {
	email := strings.ToLower(strings.TrimSpace(h.Get(f.EmailHeader)))
	user := strings.TrimSpace(h.Get(f.UserHeader))
	if email == "" {
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
