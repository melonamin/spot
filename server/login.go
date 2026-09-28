package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

const (
	loginTokenAudience = "spot-login"
	loginTokenMaxLife  = 300 * time.Second
	loginTokenLeeway   = 30 * time.Second
	minLoginSecretLen  = 32

	// sessionCookieName is used on plain HTTP (including *.localhost). HTTPS
	// uses the __Host- prefix, which a sibling site cannot set with a parent
	// Domain, so one untrusted site cannot plant a session on another.
	sessionCookieName       = "spot_session"
	secureSessionCookieName = "__Host-spot_session"
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
}

var errInvalidLoginToken = errors.New("invalid login token")

// verifyLoginToken checks a compact HS256 JWS and consumes its jti. The jti is
// marked used only after every other check passes, so a rejected token cannot
// burn a legitimate one.
func (l *DelegatedLogin) verifyLoginToken(token, host string) (Identity, error) {
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

func (l *DelegatedLogin) verifySession(value, host string) (Identity, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return Identity{}, false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(gotMAC, l.sessionMAC(payload)) {
		return Identity{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Identity{}, false
	}
	var session sessionPayload
	if err := json.Unmarshal(raw, &session); err != nil {
		return Identity{}, false
	}
	if session.Host != host || session.Email == "" || !time.Unix(session.Exp, 0).After(l.now()) {
		return Identity{}, false
	}
	groups := session.Groups
	if groups == nil {
		groups = []string{}
	}
	return Identity{Email: session.Email, Name: session.Name, Groups: groups}, true
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

// sessionCookieSecure reports whether the session cookie can be
// SameSite=None; Secure; Partitioned, which lets a cross-site preview frame
// keep it. Browsers treat *.localhost as a secure context even over HTTP.
func (s *Server) sessionCookieSecure(r *http.Request) bool {
	return s.requestScheme(r) == "https" || localSpotDomain(cleanHost(s.requestHost(r)))
}

func (s *Server) sessionCookieName(r *http.Request) string {
	if s.requestScheme(r) == "https" {
		return secureSessionCookieName
	}
	return sessionCookieName
}

func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	cookie := &http.Cookie{
		Name: s.sessionCookieName(r), Value: value, Path: "/", HttpOnly: true, MaxAge: maxAge,
		SameSite: http.SameSiteLaxMode,
	}
	if s.sessionCookieSecure(r) {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
		cookie.Partitioned = true
	}
	return cookie
}

// sessionIdentity returns the viewer signed in on this site host. Session
// cookies are honored only on site hosts; the apex has no viewer sessions.
func (s *Server) sessionIdentity(r *http.Request) (Identity, bool) {
	if s.login == nil || siteFromHost(s.requestHost(r), s.spotDomain) == "" {
		return Identity{}, false
	}
	name := s.sessionCookieName(r)
	host := s.loginHost(r)
	for _, cookie := range r.Cookies() {
		if cookie.Name != name {
			continue
		}
		if id, ok := s.login.verifySession(cookie.Value, host); ok {
			id.PeerIP = s.clientIP(r)
			return id, true
		}
	}
	return Identity{}, false
}

// safeReturnPath accepts only a local absolute path, so the auth routes can
// never redirect off the site host.
func safeReturnPath(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
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
	host := s.loginHost(r)
	id, err := s.login.verifyLoginToken(r.URL.Query().Get("token"), host)
	if err != nil {
		log.Printf("auth callback %s: %v", host, err)
		writeAuthPage(w, http.StatusBadRequest, "Sign-in failed",
			"This sign-in link is invalid, expired, or was already used. Open the site again to sign in.", "")
		return
	}
	value, err := s.login.signSession(id, host)
	if err != nil {
		log.Printf("auth callback %s: sign session: %v", host, err)
		httpError(w, http.StatusInternalServerError, "could not create the session")
		return
	}
	http.SetCookie(w, s.sessionCookie(r, value, int(s.login.sessionTTL/time.Second)))
	returnTo := safeReturnPath(r.URL.Query().Get("return_to"))
	http.Redirect(w, r, "/api/auth/check?return_to="+url.QueryEscape(returnTo), http.StatusFound)
}

// handleAuthCheck breaks the login loop when the browser drops the session
// cookie (typically third-party cookie blocking inside a frame): instead of
// redirecting to the login URL again, it asks the viewer to open a new tab.
func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	returnTo := safeReturnPath(r.URL.Query().Get("return_to"))
	if _, ok := s.sessionIdentity(r); ok {
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	siteURL := s.requestScheme(r) + "://" + s.requestHost(r) + returnTo
	writeAuthPage(w, http.StatusOK, "Open this site in a new tab",
		"Your browser did not keep the sign-in cookie for this site. Open it in a new tab.", siteURL)
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoginSiteHost(w, r) {
		return
	}
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	http.Redirect(w, r, "/", http.StatusFound)
}

func writeAuthPage(w http.ResponseWriter, status int, title, message, link string) {
	linkHTML := ""
	if link != "" {
		linkHTML = `<p><a href="` + html.EscapeString(link) + `" target="_blank" rel="noopener">Open in a new tab</a></p>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;line-height:1.5}</style></head>
<body><h1>%s</h1><p>%s</p>%s</body></html>
`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message), linkHTML)
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
func (s *Server) denyAnonymousVisitor(w http.ResponseWriter, r *http.Request) {
	if isNavigation(r) {
		target := *s.login.loginURL
		query := target.Query()
		query.Set("return_to", s.requestScheme(r)+"://"+s.requestHost(r)+r.URL.RequestURI())
		target.RawQuery = query.Encode()
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target.String(), http.StatusFound)
		return
	}
	httpError(w, http.StatusUnauthorized, "sign in required")
}

// requireVisitor gates the SDK APIs in delegated login mode: every call needs
// an identified viewer, even on open sites whose static files stay anonymous.
func (s *Server) requireVisitor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.login == nil {
			next(w, r)
			return
		}
		_, found, err := s.resolvePeer(r)
		if err != nil {
			log.Printf("visitor identity: resolve %s: %v", s.clientIP(r), err)
			httpError(w, http.StatusServiceUnavailable, "could not verify identity")
			return
		}
		if !found {
			httpError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		next(w, r)
	}
}
