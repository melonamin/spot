package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testLoginTokenSecret = "login-token-secret-0123456789abcdef"
	testSessionSecret    = "session-cookie-secret-0123456789abcdef"
	testForwardSecret    = "forward-auth-secret-0123456789"
)

func signTestLoginToken(t *testing.T, key string, header, claims map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{"alg": "HS256", "typ": "JWT"}
	}
	rawHeader, _ := json.Marshal(header)
	rawClaims, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(rawHeader) + "." + base64.RawURLEncoding.EncodeToString(rawClaims)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func testLoginClaims(host, jti string, now time.Time) map[string]any {
	return map[string]any{
		"aud": "spot-login", "host": host, "email": "Alice@Example.com", "name": "Alice",
		"groups": []string{"platform"}, "iat": now.Unix(), "exp": now.Add(60 * time.Second).Unix(), "jti": jti,
	}
}

func newTestDelegatedLogin(t *testing.T) *DelegatedLogin {
	t.Helper()
	login, err := NewDelegatedLogin("https://chat.example.com/sites/login", testLoginTokenSecret, testSessionSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return login
}

func TestVerifyLoginToken(t *testing.T) {
	now := time.Now()
	const host = "demo.sites.localhost:8443"
	for _, tt := range []struct {
		name   string
		key    string
		header map[string]any
		mutate func(map[string]any)
		host   string
	}{
		{name: "wrong key", key: "another-secret-0123456789abcdefghij"},
		{name: "alg none", header: map[string]any{"alg": "none"}},
		{name: "alg HS512", header: map[string]any{"alg": "HS512"}},
		{name: "wrong audience", mutate: func(c map[string]any) { c["aud"] = "other" }},
		{name: "other host", host: "other.sites.localhost:8443"},
		{name: "host without port", host: "demo.sites.localhost"},
		{name: "missing email", mutate: func(c map[string]any) { delete(c, "email") }},
		{name: "missing jti", mutate: func(c map[string]any) { delete(c, "jti") }},
		{name: "expired", mutate: func(c map[string]any) {
			c["iat"] = now.Add(-5 * time.Minute).Unix()
			c["exp"] = now.Add(-31 * time.Second).Unix()
		}},
		{name: "lifetime too long", mutate: func(c map[string]any) { c["exp"] = now.Add(301 * time.Second).Unix() }},
		{name: "issued in the future", mutate: func(c map[string]any) {
			c["iat"] = now.Add(2 * time.Minute).Unix()
			c["exp"] = now.Add(3 * time.Minute).Unix()
		}},
		{name: "non-numeric exp", mutate: func(c map[string]any) { c["exp"] = "later" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			login := newTestDelegatedLogin(t)
			claims := testLoginClaims(host, "jti-"+tt.name, now)
			if tt.mutate != nil {
				tt.mutate(claims)
			}
			key := testLoginTokenSecret
			if tt.key != "" {
				key = tt.key
			}
			verifyHost := host
			if tt.host != "" {
				verifyHost = tt.host
			}
			if _, err := login.verifyLoginToken(signTestLoginToken(t, key, tt.header, claims), verifyHost); err == nil {
				t.Fatal("token accepted, want rejection")
			}
		})
	}

	t.Run("valid token is single use", func(t *testing.T) {
		login := newTestDelegatedLogin(t)
		token := signTestLoginToken(t, testLoginTokenSecret, nil, testLoginClaims(host, "once", now))
		id, err := login.verifyLoginToken(token, host)
		if err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
		if id.Email != "alice@example.com" || id.Name != "Alice" || len(id.Groups) != 1 || id.Groups[0] != "platform" {
			t.Fatalf("identity = %+v", id)
		}
		if _, err := login.verifyLoginToken(token, host); err == nil {
			t.Fatal("replayed token accepted")
		}
	})

	t.Run("rejected token does not burn its jti", func(t *testing.T) {
		login := newTestDelegatedLogin(t)
		claims := testLoginClaims(host, "shared-jti", now)
		if _, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), "other.sites.localhost:8443"); err == nil {
			t.Fatal("wrong-host token accepted")
		}
		if _, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), host); err != nil {
			t.Fatalf("valid token rejected after an invalid attempt: %v", err)
		}
	})

	t.Run("seen jtis expire", func(t *testing.T) {
		login := newTestDelegatedLogin(t)
		if !login.consumeJTI("old", now.Add(time.Second), now) {
			t.Fatal("first use rejected")
		}
		login.consumeJTI("new", now.Add(time.Hour), now.Add(2*time.Second))
		if _, kept := login.usedJTIs["old"]; kept {
			t.Fatal("expired jti was not pruned")
		}
	})
}

func TestSessionCookieSignature(t *testing.T) {
	login := newTestDelegatedLogin(t)
	value, err := login.signSession(Identity{Email: "alice@example.com", Groups: []string{"platform"}}, "demo.sites.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := login.verifySession(value, "demo.sites.localhost"); !ok || id.Email != "alice@example.com" {
		t.Fatalf("valid session = %+v, %v", id, ok)
	}
	if _, ok := login.verifySession(value, "other.sites.localhost"); ok {
		t.Fatal("session accepted on another host")
	}
	payload, sig, _ := strings.Cut(value, ".")
	forged, _ := json.Marshal(sessionPayload{Email: "mallory@example.com", Host: "demo.sites.localhost", Exp: time.Now().Add(time.Hour).Unix()})
	if _, ok := login.verifySession(base64.RawURLEncoding.EncodeToString(forged)+"."+sig, "demo.sites.localhost"); ok {
		t.Fatal("forged payload accepted")
	}
	if _, ok := login.verifySession(payload, "demo.sites.localhost"); ok {
		t.Fatal("unsigned payload accepted")
	}
	login.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, ok := login.verifySession(value, "demo.sites.localhost"); ok {
		t.Fatal("expired session accepted")
	}
}

func TestSafeReturnPath(t *testing.T) {
	for raw, want := range map[string]string{
		"/":                 "/",
		"/docs/a?b=1#c":     "/docs/a?b=1#c",
		"":                  "/",
		"docs":              "/",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil.test": "/",
		"/a\r\nSet-Cookie:": "/",
	} {
		if got := safeReturnPath(raw); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// loginTestStack is a full server in delegated login mode with forward auth,
// a real registry, and local site and upload storage.
type loginTestStack struct {
	srv      *Server
	handler  http.Handler
	registry *SiteRegistry
}

func newLoginTestStack(t *testing.T) *loginTestStack {
	t.Helper()
	root := t.TempDir()
	sites, err := NewLocalSiteStore(root)
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := NewLocalFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	forwardAuth := NewForwardAuth("", "", "", "")
	forwardAuth.Secret = testForwardSecret
	srv := &Server{
		store: &DocStore{db: db, hub: NewHub()}, sites: sites, files: uploads, policies: NewPolicyStore(root, time.Minute),
		deployAuth: registry, siteAdmin: registry, siteManager: registry,
		forwardAuth: forwardAuth, login: newTestDelegatedLogin(t),
		frameAncestors: "'self' https://chat.example.com", apexRedirectURL: "https://chat.example.com/sites",
		spotDomain: "sites.localhost", trustedProxies: testTrustedProxies(t), serveStatic: true,
		deployLimit: NewRateLimiter(1000, 1000), dbLimit: NewRateLimiter(1000, 1000),
	}
	registry.SetPolicyResolver(srv.policyForSite)
	return &loginTestStack{srv: srv, handler: srv.routes(), registry: registry}
}

func (st *loginTestStack) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	st.handler.ServeHTTP(rec, req)
	return rec
}

func asForwardUser(req *http.Request, email string) *http.Request {
	req.Header.Set(forwardAuthSecretHeader, testForwardSecret)
	req.Header.Set("Remote-Email", email)
	req.Header.Set("Remote-Groups", "platform")
	return req
}

func (st *loginTestStack) deploy(t *testing.T, owner, site, access string) {
	t.Helper()
	files := map[string]string{"index.html": "<h1>" + site + "</h1>"}
	if access != "" {
		files[accessFileName] = access
	}
	rec := st.do(asForwardUser(deployRequest(t, "sites.localhost:8443", site, files), owner))
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy %s = %d %s", site, rec.Code, rec.Body.String())
	}
}

func siteRequest(method, host, path string) *http.Request {
	req := httptest.NewRequest(method, "http://spot-api"+path, nil)
	req.Header.Set("X-Forwarded-Host", host)
	return req
}

func navigation(req *http.Request) *http.Request {
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	return req
}

// signIn runs the callback on host and returns the session cookie it set.
func (st *loginTestStack) signIn(t *testing.T, host, email, jti string) *http.Cookie {
	t.Helper()
	claims := testLoginClaims(host, jti, time.Now())
	claims["email"] = email
	token := signTestLoginToken(t, testLoginTokenSecret, nil, claims)
	rec := st.do(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token+"&return_to=%2Fpage%3Fx%3D1"))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/api/auth/check?return_to=%2Fpage%3Fx%3D1" {
		t.Fatalf("callback Location = %q", got)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("callback cookies = %+v", cookies)
	}
	return cookies[0]
}

func TestDelegatedLoginRestrictedSiteFlow(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)

	rec := st.do(navigation(siteRequest(http.MethodGet, host, "/docs/?q=1")))
	if rec.Code != http.StatusFound {
		t.Fatalf("anonymous navigation = %d %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || location.Host != "chat.example.com" || location.Path != "/sites/login" ||
		location.Query().Get("return_to") != "http://"+host+"/docs/?q=1" {
		t.Fatalf("login redirect = %q", rec.Header().Get("Location"))
	}
	if rec := st.do(siteRequest(http.MethodGet, host, "/index.html")); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), "sign in required") {
		t.Fatalf("anonymous fetch = %d %s, want 401", rec.Code, rec.Body.String())
	}

	cookie := st.signIn(t, host, "owner@example.com", "owner-login")
	if cookie.Name != sessionCookieName || !cookie.HttpOnly || !cookie.Secure || !cookie.Partitioned ||
		cookie.SameSite != http.SameSiteNoneMode || cookie.Domain != "" || cookie.Path != "/" || cookie.MaxAge != 3600 {
		t.Fatalf("localhost session cookie = %+v", cookie)
	}

	check := siteRequest(http.MethodGet, host, "/api/auth/check?return_to=%2Fpage%3Fx%3D1")
	check.AddCookie(cookie)
	if rec := st.do(check); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/page?x=1" {
		t.Fatalf("check with cookie = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	page := navigation(siteRequest(http.MethodGet, host, "/"))
	page.AddCookie(cookie)
	if rec := st.do(page); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>private</h1>") {
		t.Fatalf("signed-in page = %d %s", rec.Code, rec.Body.String())
	}

	// A signed-in viewer outside the allowlist is forbidden, not sent to login.
	stranger := st.signIn(t, host, "stranger@example.com", "stranger-login")
	page = navigation(siteRequest(http.MethodGet, host, "/"))
	page.AddCookie(stranger)
	if rec := st.do(page); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger page = %d, want 403", rec.Code)
	}

	// A session cookie is bound to the host it was issued for.
	st.deploy(t, "owner@example.com", "other", `{"allow":["owner@example.com"]}`)
	elsewhere := siteRequest(http.MethodGet, "other.sites.localhost:8443", "/")
	elsewhere.AddCookie(cookie)
	if rec := st.do(elsewhere); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie replay on another site = %d, want 401", rec.Code)
	}

	logout := siteRequest(http.MethodGet, host, "/api/auth/logout")
	logout.AddCookie(cookie)
	rec = st.do(logout)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("logout = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if cleared := rec.Result().Cookies(); len(cleared) != 1 || cleared[0].MaxAge >= 0 || !cleared[0].Partitioned {
		t.Fatalf("logout cookie = %+v", cleared)
	}
}

func TestDelegatedLoginCallbackRejectsBadInput(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)

	rec := st.do(siteRequest(http.MethodGet, host, "/api/auth/callback?token=garbage&return_to=/"))
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("invalid token = %d %v", rec.Code, rec.Header())
	}

	// A token minted for another site cannot sign in here.
	token := signTestLoginToken(t, testLoginTokenSecret, nil, testLoginClaims("other.sites.localhost:8443", "cross", time.Now()))
	if rec := st.do(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token)); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-host token = %d, want 400", rec.Code)
	}

	// An open redirect target falls back to the site root.
	claims := testLoginClaims(host, "redirect", time.Now())
	token = signTestLoginToken(t, testLoginTokenSecret, nil, claims)
	rec = st.do(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token+"&return_to=%2F%2Fevil.example"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/api/auth/check?return_to=%2F" {
		t.Fatalf("open redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Auth routes exist only on site hosts.
	if rec := st.do(siteRequest(http.MethodGet, "sites.localhost:8443", "/api/auth/callback?token="+token)); rec.Code != http.StatusNotFound {
		t.Fatalf("apex callback = %d, want 404", rec.Code)
	}
}

func TestDelegatedLoginCheckPageBreaksLoop(t *testing.T) {
	st := newLoginTestStack(t)
	rec := st.do(siteRequest(http.MethodGet, "demo.sites.localhost:8443", "/api/auth/check?return_to=%2Fa%3Cb"))
	if rec.Code != http.StatusOK {
		t.Fatalf("check without cookie = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "did not keep the sign-in cookie") || !strings.Contains(body, `target="_blank"`) ||
		!strings.Contains(body, `href="http://demo.sites.localhost:8443/a&lt;b"`) {
		t.Fatalf("check page = %s", body)
	}
}

func TestSessionCookieModes(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)

	https := siteRequest(http.MethodGet, "private.sites.localhost", "/api/auth/callback")
	https.Header.Set("X-Forwarded-Proto", "https")
	if got := st.srv.sessionCookie(https, "v", 60); got.Name != secureSessionCookieName || !got.Secure || !got.Partitioned || got.SameSite != http.SameSiteNoneMode {
		t.Fatalf("https cookie = %+v", got)
	}

	st.srv.spotDomain = "sites.example.com"
	plain := siteRequest(http.MethodGet, "private.sites.example.com", "/api/auth/callback")
	if got := st.srv.sessionCookie(plain, "v", 60); got.Name != sessionCookieName || got.Secure || got.Partitioned || got.SameSite != http.SameSiteLaxMode {
		t.Fatalf("plain http cookie = %+v", got)
	}

	// The login host drops only the scheme's default port.
	for _, tt := range []struct{ host, proto, want string }{
		{"Demo.Sites.Example.com.", "https", "demo.sites.example.com"},
		{"demo.sites.example.com:443", "https", "demo.sites.example.com"},
		{"demo.sites.example.com:80", "http", "demo.sites.example.com"},
		{"demo.sites.example.com:8443", "https", "demo.sites.example.com:8443"},
		{"demo.sites.example.com:443", "http", "demo.sites.example.com:443"},
	} {
		req := siteRequest(http.MethodGet, tt.host, "/")
		req.Header.Set("X-Forwarded-Proto", tt.proto)
		if got := st.srv.loginHost(req); got != tt.want {
			t.Errorf("loginHost(%s %s) = %q, want %q", tt.proto, tt.host, got, tt.want)
		}
	}
}

func TestDelegatedLoginGatesSDKAPIsOnOpenSites(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "open.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "open", "")

	if rec := st.do(navigation(siteRequest(http.MethodGet, host, "/"))); rec.Code != http.StatusOK {
		t.Fatalf("anonymous static page on open site = %d, want 200", rec.Code)
	}
	for _, path := range []string{"/api/db/posts", "/api/files", "/api/me", "/api/ws"} {
		if rec := st.do(siteRequest(http.MethodGet, host, path)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d %s, want 401", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/api/ai/chat", "/api/slack/send"} {
		if rec := st.do(siteRequest(http.MethodPost, host, path)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST %s = %d, want 401", path, rec.Code)
		}
	}

	cookie := st.signIn(t, host, "viewer@example.com", "viewer")
	req := siteRequest(http.MethodGet, host, "/api/db/posts")
	req.AddCookie(cookie)
	if rec := st.do(req); rec.Code != http.StatusOK {
		t.Fatalf("signed-in db list = %d %s", rec.Code, rec.Body.String())
	}
	req = siteRequest(http.MethodGet, host, "/api/me")
	req.AddCookie(cookie)
	rec := st.do(req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"viewer@example.com"`) {
		t.Fatalf("signed-in /api/me = %d %s", rec.Code, rec.Body.String())
	}
	req = siteRequest(http.MethodGet, host, "/api/db/shared-posts")
	req.AddCookie(cookie)
	if rec := st.do(req); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "shared-* collections are disabled") {
		t.Fatalf("shared collection = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestSharedScopesStayEnabledWithoutDelegatedLogin(t *testing.T) {
	srv := &Server{}
	if scope, err := srv.collectionScope("demo", "shared-posts"); err != nil || scope != sharedScope {
		t.Fatalf("collection scope = %q, %v", scope, err)
	}
	if scope, err := srv.roomScope("demo", "shared-room"); err != nil || scope != sharedScope {
		t.Fatalf("room scope = %q, %v", scope, err)
	}
	srv.login = &DelegatedLogin{}
	if _, err := srv.roomScope("demo", "shared-room"); err == nil {
		t.Fatal("shared room allowed in delegated login mode")
	}
	if scope, err := srv.roomScope("demo", "lobby"); err != nil || scope != "demo" {
		t.Fatalf("private room scope = %q, %v", scope, err)
	}
}

func TestFrameAncestorsAndApexRedirect(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open", "")

	rec := st.do(siteRequest(http.MethodGet, "open.sites.localhost:8443", "/"))
	if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 || got[0] != "frame-ancestors 'self' https://chat.example.com" {
		t.Fatalf("site CSP = %q", got)
	}
	if rec.Header().Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Fatalf("site Referrer-Policy = %q", rec.Header().Get("Referrer-Policy"))
	}

	for _, path := range []string{"/", "/spots", "/gallery/", "/stats", "/help", "/index.html"} {
		rec := st.do(siteRequest(http.MethodGet, "sites.localhost:8443", path))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://chat.example.com/sites" {
			t.Fatalf("apex %s = %d %q, want redirect", path, rec.Code, rec.Header().Get("Location"))
		}
		if rec.Header().Get("Content-Security-Policy") != "" {
			t.Fatalf("apex %s carries site CSP", path)
		}
	}
	for _, path := range []string{"/agent.md", "/spot.js", "/health"} {
		if rec := st.do(siteRequest(http.MethodGet, "sites.localhost:8443", path)); rec.Code != http.StatusOK {
			t.Fatalf("apex %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestTLSAsk(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "live", "")
	for domain, want := range map[string]int{
		"sites.localhost":            http.StatusOK,
		"live.sites.localhost":       http.StatusOK,
		"LIVE.sites.localhost.":      http.StatusOK,
		"missing.sites.localhost":    http.StatusNotFound,
		"a.live.sites.localhost":     http.StatusNotFound,
		"live.sites.localhost:443":   http.StatusNotFound,
		"live.example.com":           http.StatusNotFound,
		"":                           http.StatusNotFound,
		"sites.localhost.evil.test":  http.StatusNotFound,
		"live.sites.localhost.evil.": http.StatusNotFound,
	} {
		// Caddy calls the service by its internal name, not a Spot host.
		req := httptest.NewRequest(http.MethodGet, "http://sites:8080/api/tls/ask?domain="+url.QueryEscape(domain), nil)
		if rec := st.do(req); rec.Code != want {
			t.Errorf("tls ask %q = %d, want %d", domain, rec.Code, want)
		}
	}
	if rec := st.do(httptest.NewRequest(http.MethodGet, "http://sites:8080/api/sites/visible", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("other API on internal host = %d, want 400", rec.Code)
	}
}

func TestDelegatedLoginAcceptsForwardAuthFirst(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	req := asForwardUser(siteRequest(http.MethodGet, "private.sites.localhost:8443", "/"), "owner@example.com")
	if rec := st.do(req); rec.Code != http.StatusOK {
		t.Fatalf("forward-auth owner = %d %s", rec.Code, rec.Body.String())
	}
	id, found, err := st.srv.resolvePeer(asForwardUser(siteRequest(http.MethodGet, "sites.localhost:8443", "/"), "owner@example.com"))
	if err != nil || !found || id.Email != "owner@example.com" {
		t.Fatalf("apex forward-auth identity = %+v %v %v", id, found, err)
	}
}

// A malicious site must not read another site's uploads through its own
// origin with the visitor's session: identity on a site host belongs to that
// host's site only.
func TestFileDownloadIsBoundToTheSiteHost(t *testing.T) {
	st := newLoginTestStack(t)
	const (
		evilHost   = "evil.sites.localhost:8443"
		victimHost = "victim.sites.localhost:8443"
	)
	st.deploy(t, "mallory@example.com", "evil", "")
	st.deploy(t, "owner@example.com", "victim", `{"allow":["alice@example.com"]}`)
	secret, err := st.srv.files.Put(context.Background(), "victim", "secret.txt", "text/plain", strings.NewReader("victim secret"), 13)
	if err != nil {
		t.Fatal(err)
	}
	own, err := st.srv.files.Put(context.Background(), "evil", "own.txt", "text/plain", strings.NewReader("evil file"), 9)
	if err != nil {
		t.Fatal(err)
	}
	evilSession := st.signIn(t, evilHost, "alice@example.com", "alice-evil")
	victimSession := st.signIn(t, victimHost, "alice@example.com", "alice-victim")

	download := func(host, url string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := siteRequest(http.MethodGet, host, url)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		return st.do(req)
	}

	// Denied: the evil origin fetching the victim's upload with the visitor's
	// evil-host session, or with an ambient forward-auth identity.
	if rec := download(evilHost, secret.URL, evilSession); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "victim secret") {
		t.Fatalf("cross-site download with session = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := st.do(asForwardUser(siteRequest(http.MethodGet, evilHost, secret.URL), "alice@example.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-site download with forward auth = %d, want 404", rec.Code)
	}
	// A site-host session confers nothing on the apex.
	if rec := download("sites.localhost:8443", secret.URL, victimSession); rec.Code != http.StatusUnauthorized {
		t.Fatalf("apex download with a site session = %d, want 401", rec.Code)
	}

	// Allowed: each site's own uploads on its own host, and the apex with a
	// platform identity.
	if rec := download(victimHost, secret.URL, victimSession); rec.Code != http.StatusOK || rec.Body.String() != "victim secret" {
		t.Fatalf("same-site download = %d %s", rec.Code, rec.Body.String())
	}
	if rec := download(evilHost, own.URL, evilSession); rec.Code != http.StatusOK || rec.Body.String() != "evil file" {
		t.Fatalf("own-site download = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(asForwardUser(siteRequest(http.MethodGet, "sites.localhost:8443", secret.URL), "alice@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("apex download with forward auth = %d %s", rec.Code, rec.Body.String())
	}
}
