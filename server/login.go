package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Delegated login lets an external application sign viewers in to site hosts.
// Spot redirects a restricted-site navigation to SPOT_LOGIN_URL; the external
// app authenticates the browser and sends it back to /api/auth/callback on the
// same site host with a short HS256 login token. Spot then issues its own
// signed, host-only session cookie, so each site host has a separate session.
//
// The redirect carries a random state that Spot also stores in a host-only
// cookie; the token must echo it. Without that binding anyone could mint a
// token for their own account and sign a victim's browser in as themselves.

const (
	loginTokenAudience = "spot-login"
	loginTokenMaxLife  = 300 * time.Second
	loginTokenLeeway   = 30 * time.Second
	minLoginSecretLen  = 32

	// Delegated login cookies always use the __Host- prefix, which browsers
	// accept over HTTPS and on *.localhost (a secure context even over HTTP).
	// A sibling site cannot set a __Host- cookie with a parent Domain, so one
	// untrusted site cannot plant a session or login state on another.
	sessionCookieName    = "__Host-spot_session"
	loginStateCookieName = "__Host-spot_login_state"
	loginStateMaxAge     = 10 * time.Minute
	loginStateBytes      = 32
)

type DelegatedLogin struct {
	loginURL   *url.URL
	tokenKey   []byte
	sessionKey []byte
	sessionTTL time.Duration
	now        func() time.Time

	mu       sync.Mutex
	usedJTIs map[string]time.Time
}

func NewDelegatedLogin(loginURL, tokenSecret, sessionSecret string, ttl time.Duration) (*DelegatedLogin, error) {
	u, err := parseAbsoluteURL(loginURL)
	if err != nil {
		return nil, fmt.Errorf("SPOT_LOGIN_URL: %w", err)
	}
	return &DelegatedLogin{
		loginURL:   u,
		tokenKey:   []byte(tokenSecret),
		sessionKey: []byte(sessionSecret),
		sessionTTL: ttl,
		now:        time.Now,
		usedJTIs:   map[string]time.Time{},
	}, nil
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}
	return u, nil
}

type loginTokenClaims struct {
	Aud    string   `json:"aud"`
	Host   string   `json:"host"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
	JTI    string   `json:"jti"`
	State  string   `json:"state"`
}

var errInvalidLoginToken = errors.New("invalid login token")

// verifyLoginToken checks a compact HS256 JWS against the host and the
// browser's login state, and consumes its jti. The jti is marked used only
// after every other check passes, so a rejected token cannot burn a
// legitimate one.
func (l *DelegatedLogin) verifyLoginToken(token, host, state string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, fmt.Errorf("%w: malformed", errInvalidLoginToken)
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, fmt.Errorf("%w: header encoding", errInvalidLoginToken)
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil || header.Alg != "HS256" {
		return Identity{}, fmt.Errorf("%w: unsupported algorithm", errInvalidLoginToken)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, fmt.Errorf("%w: signature encoding", errInvalidLoginToken)
	}
	mac := hmac.New(sha256.New, l.tokenKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Identity{}, fmt.Errorf("%w: bad signature", errInvalidLoginToken)
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("%w: claims encoding", errInvalidLoginToken)
	}
	var claims loginTokenClaims
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return Identity{}, fmt.Errorf("%w: claims: %v", errInvalidLoginToken, err)
	}
	now := l.now()
	switch {
	case claims.Aud != loginTokenAudience:
		return Identity{}, fmt.Errorf("%w: audience", errInvalidLoginToken)
	case claims.Host == "" || claims.Host != host:
		return Identity{}, fmt.Errorf("%w: token is for host %q, not %q", errInvalidLoginToken, claims.Host, host)
	case strings.TrimSpace(claims.Email) == "":
		return Identity{}, fmt.Errorf("%w: email required", errInvalidLoginToken)
	case claims.JTI == "":
		return Identity{}, fmt.Errorf("%w: jti required", errInvalidLoginToken)
	case state == "" || subtle.ConstantTimeCompare([]byte(claims.State), []byte(state)) != 1:
		return Identity{}, fmt.Errorf("%w: state does not match this browser", errInvalidLoginToken)
	case claims.Iat == 0 || claims.Exp <= claims.Iat || claims.Exp-claims.Iat > int64(loginTokenMaxLife/time.Second):
		return Identity{}, fmt.Errorf("%w: lifetime", errInvalidLoginToken)
	case time.Unix(claims.Iat, 0).After(now.Add(loginTokenLeeway)):
		return Identity{}, fmt.Errorf("%w: issued in the future", errInvalidLoginToken)
	case !time.Unix(claims.Exp, 0).Add(loginTokenLeeway).After(now):
		return Identity{}, fmt.Errorf("%w: expired", errInvalidLoginToken)
	}
	if !l.consumeJTI(claims.JTI, time.Unix(claims.Exp, 0).Add(loginTokenLeeway), now) {
		return Identity{}, fmt.Errorf("%w: already used", errInvalidLoginToken)
	}
	groups := claims.Groups
	if groups == nil {
		groups = []string{}
	}
	return Identity{
		Email:  strings.ToLower(strings.TrimSpace(claims.Email)),
		Name:   strings.TrimSpace(claims.Name),
		Groups: groups,
	}, nil
}

// consumeJTI is a process-local replay guard. Spot runs as one process, and a
// token outlives its seen-set entry by at most the leeway already applied.
func (l *DelegatedLogin) consumeJTI(jti string, expiresAt, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for seen, until := range l.usedJTIs {
		if !until.After(now) {
			delete(l.usedJTIs, seen)
		}
	}
	if _, used := l.usedJTIs[jti]; used {
		return false
	}
	l.usedJTIs[jti] = expiresAt
	return true
}

type sessionPayload struct {
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
	Host   string   `json:"host"`
	Exp    int64    `json:"exp"`
}

func (l *DelegatedLogin) signSession(id Identity, host string) (string, error) {
	raw, err := json.Marshal(sessionPayload{
		Email: id.Email, Name: id.Name, Groups: id.Groups, Host: host,
		Exp: l.now().Add(l.sessionTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(l.sessionMAC(payload)), nil
}

func (l *DelegatedLogin) sessionMAC(payload string) []byte {
	mac := hmac.New(sha256.New, l.sessionKey)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// verifySession checks a session cookie value for host and returns its
// identity and expiry.
func (l *DelegatedLogin) verifySession(value, host string) (Identity, time.Time, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return Identity{}, time.Time{}, false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(gotMAC, l.sessionMAC(payload)) {
		return Identity{}, time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Identity{}, time.Time{}, false
	}
	var session sessionPayload
	if err := json.Unmarshal(raw, &session); err != nil {
		return Identity{}, time.Time{}, false
	}
	expires := time.Unix(session.Exp, 0)
	if session.Host != host || session.Email == "" || !expires.After(l.now()) {
		return Identity{}, time.Time{}, false
	}
	groups := session.Groups
	if groups == nil {
		groups = []string{}
	}
	return Identity{Email: session.Email, Name: session.Name, Groups: groups}, expires, true
}

// loginHost is the site host as the browser addressed it: lowercase, without
// a trailing dot, keeping the port unless it is the scheme's default. Login
// tokens and session cookies are bound to this exact value.
func (s *Server) loginHost(r *http.Request) string {
	host := strings.ToLower(s.requestHost(r))
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(host, ".")
	scheme := s.requestScheme(r)
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return host
	}
	return net.JoinHostPort(host, port)
}

// loginTransportSecure reports whether delegated login may run on this
// request: over HTTPS, or on *.localhost, which browsers treat as a secure
// context even over HTTP. Elsewhere a sibling site could plant cookies.
func (s *Server) loginTransportSecure(r *http.Request) bool {
	return s.requestScheme(r) == "https" || localSpotDomain(cleanHost(s.requestHost(r)))
}

// loginCookie builds a host-only delegated login cookie. SameSite=None with
// Partitioned lets a cross-site preview frame keep it. __Host- names require
// Secure, Path=/ and no Domain, which this always sets.
func loginCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, MaxAge: maxAge,
		SameSite: http.SameSiteNoneMode, Secure: true, Partitioned: true,
	}
}

func sessionCookie(value string, maxAge int) *http.Cookie {
	return loginCookie(sessionCookieName, value, maxAge)
}

// sessionIdentity returns the viewer signed in on this site host. Session
// cookies are honored only on site hosts; the apex has no viewer sessions.
// A request carrying more than one session cookie is anonymous: the browser's
// ordering would choose the identity, and a browser that does not enforce the
// __Host- prefix could let a sibling site add one.
func (s *Server) sessionIdentity(r *http.Request) (Identity, bool) {
	id, _, ok := s.verifiedSession(r)
	return id, ok
}

func (s *Server) verifiedSession(r *http.Request) (Identity, time.Time, bool) {
	if s.login == nil || siteFromHost(s.requestHost(r), s.spotDomain) == "" || !s.loginTransportSecure(r) {
		return Identity{}, time.Time{}, false
	}
	values := s.sessionCookieValues(r)
	if len(values) != 1 {
		return Identity{}, time.Time{}, false
	}
	id, expires, ok := s.login.verifySession(values[0], s.loginHost(r))
	if !ok {
		return Identity{}, time.Time{}, false
	}
	id.PeerIP = s.clientIP(r)
	return id, expires, true
}

// sessionExpiry reports when the request's identity lapses, if it came from
// a delegated session. It reads the identity requireVisitor resolved, so a
// session that expires in between cannot leave a connection without a
// deadline.
func (s *Server) sessionExpiry(r *http.Request) (time.Time, bool) {
	if s.login == nil {
		return time.Time{}, false
	}
	_, expires, found, err := s.resolvePeerSession(r)
	return expires, found && err == nil && !expires.IsZero()
}

func (s *Server) sessionCookieValues(r *http.Request) []string {
	return cookieValues(r, sessionCookieName)
}

func cookieValues(r *http.Request, name string) []string {
	var values []string
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			values = append(values, cookie.Value)
		}
	}
	return values
}

const duplicateSessionCookiesMessage = "sign in required: the request carries more than one Spot session cookie, " +
	"so another site may have set one; clear this site's cookies and sign in again"

// hasDuplicateSessionCookies reports a site-host request that
// sessionIdentity refuses because of conflicting session cookies.
func (s *Server) hasDuplicateSessionCookies(r *http.Request) bool {
	return s.login != nil && siteFromHost(s.requestHost(r), s.spotDomain) != "" && len(s.sessionCookieValues(r)) > 1
}

// signInRequiredMessage is the 401 text for an unidentified site visitor.
func (s *Server) signInRequiredMessage(r *http.Request) string {
	if s.hasDuplicateSessionCookies(r) {
		return duplicateSessionCookiesMessage
	}
	return "sign in required"
}

// safeReturnPath accepts only a local absolute path, so the auth routes can
// never redirect off the site host. Backslashes are refused anywhere:
// http.Redirect cleans "/a/../\\host" to "/\\host", which browsers read as
// "//host".
func safeReturnPath(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsRune(raw, '\\') {
		return "/"
	}
	for _, c := range raw {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return raw
}

func (s *Server) requireLoginSiteHost(w http.ResponseWriter, r *http.Request) bool {
	if s.login == nil || siteFromHost(s.requestHost(r), s.spotDomain) == "" {
		http.NotFound(w, r)
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	return true
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	if !s.loginTransportSecure(r) {
		writeStatusPage(w, http.StatusForbidden, insecureLoginPage)
		return
	}
	host := s.loginHost(r)
	returnTo := safeReturnPath(r.URL.Query().Get("return_to"))
	states := cookieValues(r, loginStateCookieName)
	if len(states) == 0 {
		// Another tab's callback already used the shared state and signed
		// this browser in; the token here stays unused.
		if _, ok := s.sessionIdentity(r); ok {
			http.Redirect(w, r, "/api/auth/check?return_to="+url.QueryEscape(returnTo), http.StatusFound)
			return
		}
		// The browser dropped the state cookie (third-party cookie blocking
		// in a frame), or it never started this sign-in.
		s.writeOpenInNewTab(w, r, returnTo)
		return
	}
	state := ""
	if len(states) == 1 {
		state = states[0]
	}
	id, err := s.login.verifyLoginToken(r.URL.Query().Get("token"), host, state)
	if err != nil {
		log.Printf("auth callback %s: %v", host, err)
		writeStatusPage(w, http.StatusBadRequest, statusPage{
			Title:   "Sign-in failed",
			Message: "This sign-in link is invalid, expired, or was already used. Open the site again to sign in.",
			Links:   []statusPageLink{{Label: "Open the site again", URL: "/", Primary: true}},
		})
		return
	}
	value, err := s.login.signSession(id, host)
	if err != nil {
		log.Printf("auth callback %s: sign session: %v", host, err)
		httpError(w, http.StatusInternalServerError, "could not create the session")
		return
	}
	http.SetCookie(w, loginCookie(loginStateCookieName, "", -1))
	http.SetCookie(w, sessionCookie(value, int(s.login.sessionTTL/time.Second)))
	http.Redirect(w, r, "/api/auth/check?return_to="+url.QueryEscape(returnTo), http.StatusFound)
}

var insecureLoginPage = statusPage{
	Title:   "Sign-in needs a secure connection",
	Message: "This Spot deployment signs visitors in only over HTTPS. Ask its operator to serve sites over HTTPS.",
}

// writeOpenInNewTab ends a login loop when the browser drops Spot's cookies,
// typically third-party cookie blocking inside a frame.
func (s *Server) writeOpenInNewTab(w http.ResponseWriter, r *http.Request, returnTo string) {
	siteURL := s.requestScheme(r) + "://" + s.requestHost(r) + returnTo
	writeStatusPage(w, http.StatusOK, statusPage{
		Title:   "Open this site in a new tab",
		Message: "Your browser did not keep the sign-in cookie for this site. This happens in some embedded previews.",
		Links:   []statusPageLink{{Label: "Open in a new tab", URL: siteURL, NewTab: true, Primary: true}},
	})
}

// handleAuthCheck breaks the login loop when the browser drops the session
// cookie: instead of redirecting to the login URL again, it asks the viewer
// to open a new tab.
func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	if !s.loginTransportSecure(r) {
		writeStatusPage(w, http.StatusForbidden, insecureLoginPage)
		return
	}
	returnTo := safeReturnPath(r.URL.Query().Get("return_to"))
	if s.hasDuplicateSessionCookies(r) {
		writeStatusPage(w, http.StatusBadRequest, statusPage{
			Title:   "Sign-in failed",
			Message: "Your browser sent more than one session cookie for this site, so another site may have set one. Clear this site's cookies and open it again.",
		})
		return
	}
	if _, ok := s.sessionIdentity(r); ok {
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	s.writeOpenInNewTab(w, r, returnTo)
}

// handleAuthLogout signs the viewer out. Only a page load from this site, or
// one the viewer typed, may do it, so another page cannot sign a viewer out
// with an image, a fetch, or a link. That needs Sec-Fetch-Site, which every
// current browser sends; without it the request is refused.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); !isNavigation(r) || (site != "same-origin" && site != "none") {
		httpError(w, http.StatusForbidden, "sign out by opening /api/auth/logout in the browser")
		return
	}
	http.SetCookie(w, sessionCookie("", -1))
	http.Redirect(w, r, "/", http.StatusFound)
}

// isNavigation reports whether a request is a browser page load, which gets a
// login redirect instead of a JSON 401.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html")
}

// denyAnonymousVisitor answers a site request that needs a signed-in viewer.
// A page load on a site host starts a sign-in; anything else, including the
// apex, which has no viewer sessions, gets a JSON 401.
func (s *Server) denyAnonymousVisitor(w http.ResponseWriter, r *http.Request) {
	if !isNavigation(r) || siteFromHost(s.requestHost(r), s.spotDomain) == "" {
		httpError(w, http.StatusUnauthorized, s.signInRequiredMessage(r))
		return
	}
	s.startLogin(w, r, r.URL.RequestURI())
}

// handleAuthLogin starts a sign-in on request. Open sites serve their pages
// anonymously, so a page that needs the SDK APIs sends the browser here.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	s.startLogin(w, r, safeReturnPath(r.URL.Query().Get("return_to")))
}

// startLogin gives the browser a login state cookie and redirects it to the
// login URL carrying that state, returning to returnPath on this host.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request, returnPath string) {
	if !s.loginTransportSecure(r) {
		writeStatusPage(w, http.StatusForbidden, insecureLoginPage)
		return
	}
	state, err := s.loginState(r)
	if err != nil {
		log.Printf("login state: %v", err)
		httpError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	http.SetCookie(w, loginCookie(loginStateCookieName, state, int(loginStateMaxAge/time.Second)))
	target := *s.login.loginURL
	query := target.Query()
	query.Set("return_to", s.requestScheme(r)+"://"+s.requestHost(r)+returnPath)
	query.Set("state", state)
	target.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// loginState reuses the browser's pending login state, so sign-ins started
// in several tabs can all complete, or creates a new one.
func (s *Server) loginState(r *http.Request) (string, error) {
	if values := cookieValues(r, loginStateCookieName); len(values) == 1 {
		if raw, err := base64.RawURLEncoding.DecodeString(values[0]); err == nil && len(raw) == loginStateBytes {
			return values[0], nil
		}
	}
	raw := make([]byte, loginStateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// requireVisitor gates the SDK APIs in delegated login mode: every call needs
// an identified viewer, even on open sites whose static files stay anonymous.
func (s *Server) requireVisitor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.login == nil {
			next(w, r)
			return
		}
		id, expires, found, err := s.resolvePeerSession(r)
		if err != nil {
			log.Printf("visitor identity: resolve %s: %v", s.clientIP(r), err)
			httpError(w, http.StatusServiceUnavailable, "could not verify identity")
			return
		}
		if !found {
			// A page load, such as an opened upload link, starts a sign-in.
			s.denyAnonymousVisitor(w, r)
			return
		}
		next(w, withResolvedPeer(r, id, expires))
	}
}

type resolvedPeerKey struct{}

type resolvedPeer struct {
	id      Identity
	expires time.Time // zero unless the identity came from a delegated session
}

// withResolvedPeer records the identity already resolved for this request, so
// the handler's own resolvePeer does not ask the mesh resolver again, and
// both see the same session expiry.
func withResolvedPeer(r *http.Request, id Identity, expires time.Time) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), resolvedPeerKey{}, resolvedPeer{id: id, expires: expires}))
}
