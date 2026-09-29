package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	testLoginTokenSecret = "login-token-secret-0123456789abcdef"
	testSessionSecret    = "session-cookie-secret-0123456789abcdef"
	testForwardSecret    = "forward-auth-secret-0123456789"
	// testLoginState is a well-formed login state: 32 bytes, base64url.
	testLoginState  = "c3BvdC1sb2dpbi1zdGF0ZS0wMTIzNDU2Nzg5YWJjZGU"
	otherLoginState = "b3RoZXItbG9naW4tc3RhdGUtMDEyMzQ1Njc4OWFiY2Q"
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
		"state": testLoginState,
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
		{name: "missing state", mutate: func(c map[string]any) { delete(c, "state") }},
		{name: "other browser's state", mutate: func(c map[string]any) { c["state"] = otherLoginState }},
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
			if _, err := login.verifyLoginToken(signTestLoginToken(t, key, tt.header, claims), verifyHost, testLoginState); err == nil {
				t.Fatal("token accepted, want rejection")
			}
		})
	}

	t.Run("valid token is single use", func(t *testing.T) {
		login := newTestDelegatedLogin(t)
		token := signTestLoginToken(t, testLoginTokenSecret, nil, testLoginClaims(host, "once", now))
		if _, err := login.verifyLoginToken(token, host, ""); err == nil {
			t.Fatal("token accepted without a browser state")
		}
		id, err := login.verifyLoginToken(token, host, testLoginState)
		if err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
		if id.Email != "alice@example.com" || id.Name != "Alice" || len(id.Groups) != 1 || id.Groups[0] != "platform" {
			t.Fatalf("identity = %+v", id)
		}
		if _, err := login.verifyLoginToken(token, host, testLoginState); err == nil {
			t.Fatal("replayed token accepted")
		}
	})

	t.Run("rejected token does not burn its jti", func(t *testing.T) {
		login := newTestDelegatedLogin(t)
		claims := testLoginClaims(host, "shared-jti", now)
		if _, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), "other.sites.localhost:8443", testLoginState); err == nil {
			t.Fatal("wrong-host token accepted")
		}
		if _, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), host, otherLoginState); err == nil {
			t.Fatal("token accepted for another browser's state")
		}
		if _, err := login.verifyLoginToken(signTestLoginToken(t, testLoginTokenSecret, nil, claims), host, testLoginState); err != nil {
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
	if id, expires, ok := login.verifySession(value, "demo.sites.localhost"); !ok || id.Email != "alice@example.com" ||
		expires.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("valid session = %+v, %v, expires %v", id, ok, expires)
	}
	if _, _, ok := login.verifySession(value, "other.sites.localhost"); ok {
		t.Fatal("session accepted on another host")
	}
	payload, sig, _ := strings.Cut(value, ".")
	forged, _ := json.Marshal(sessionPayload{Email: "mallory@example.com", Host: "demo.sites.localhost", Exp: time.Now().Add(time.Hour).Unix()})
	if _, _, ok := login.verifySession(base64.RawURLEncoding.EncodeToString(forged)+"."+sig, "demo.sites.localhost"); ok {
		t.Fatal("forged payload accepted")
	}
	if _, _, ok := login.verifySession(payload, "demo.sites.localhost"); ok {
		t.Fatal("unsigned payload accepted")
	}
	login.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, _, ok := login.verifySession(value, "demo.sites.localhost"); ok {
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
		// http.Redirect would clean this to "/\\evil.example" ("//evil.example").
		"/a/../\\evil.example": "/",
		"/docs\\page":          "/",
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

// withLoginState adds the login state cookie a login redirect would have set.
func withLoginState(req *http.Request) *http.Request {
	req.AddCookie(&http.Cookie{Name: loginStateCookieName, Value: testLoginState})
	return req
}

// responseCookie returns the named cookie a response set.
func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set %s: %v", name, rec.Result().Cookies())
	return nil
}

// signIn runs the callback on host and returns the session cookie it set.
func (st *loginTestStack) signIn(t *testing.T, host, email, jti string) *http.Cookie {
	t.Helper()
	claims := testLoginClaims(host, jti, time.Now())
	claims["email"] = email
	token := signTestLoginToken(t, testLoginTokenSecret, nil, claims)
	rec := st.do(withLoginState(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token+"&return_to=%2Fpage%3Fx%3D1")))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/api/auth/check?return_to=%2Fpage%3Fx%3D1" {
		t.Fatalf("callback Location = %q", got)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 2 {
		t.Fatalf("callback cookies = %+v, want the session and a cleared login state", cookies)
	}
	if state := responseCookie(t, rec, loginStateCookieName); state.MaxAge >= 0 {
		t.Fatalf("login state cookie after sign-in = %+v, want cleared", state)
	}
	return responseCookie(t, rec, sessionCookieName)
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
	state := responseCookie(t, rec, loginStateCookieName)
	if state.Value == "" || location.Query().Get("state") != state.Value || !state.HttpOnly || !state.Secure ||
		!state.Partitioned || state.SameSite != http.SameSiteNoneMode || state.Domain != "" || state.Path != "/" || state.MaxAge != 600 {
		t.Fatalf("login state cookie = %+v, redirect state = %q", state, location.Query().Get("state"))
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

	// Only a page load from this site signs out, so another page cannot do it
	// with an image or a link.
	link := navigation(siteRequest(http.MethodGet, host, "/api/auth/logout"))
	link.Header.Set("Sec-Fetch-Site", "cross-site")
	link.AddCookie(cookie)
	if rec := st.do(link); rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("logout from another site's link = %d %v, want 403", rec.Code, rec.Result().Cookies())
	}
	image := siteRequest(http.MethodGet, host, "/api/auth/logout")
	image.Header.Set("Sec-Fetch-Mode", "no-cors")
	image.Header.Set("Accept", "image/avif,image/webp,*/*")
	image.AddCookie(cookie)
	if rec := st.do(image); rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("logout from an image = %d %v, want 403", rec.Code, rec.Result().Cookies())
	}
	logout := navigation(siteRequest(http.MethodGet, host, "/api/auth/logout"))
	logout.Header.Set("Sec-Fetch-Site", "same-origin")
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

	rec := st.do(withLoginState(siteRequest(http.MethodGet, host, "/api/auth/callback?token=garbage&return_to=/")))
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("invalid token = %d %v", rec.Code, rec.Header())
	}

	// A token minted for another site cannot sign in here.
	token := signTestLoginToken(t, testLoginTokenSecret, nil, testLoginClaims("other.sites.localhost:8443", "cross", time.Now()))
	if rec := st.do(withLoginState(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token))); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-host token = %d, want 400", rec.Code)
	}

	// An open redirect target falls back to the site root.
	claims := testLoginClaims(host, "redirect", time.Now())
	token = signTestLoginToken(t, testLoginTokenSecret, nil, claims)
	rec = st.do(withLoginState(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token+"&return_to=%2F%2Fevil.example")))
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
		!strings.Contains(body, `href="http://demo.sites.localhost:8443/a%3cb"`) {
		t.Fatalf("check page = %s", body)
	}
}

func TestSessionCookieModes(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)

	// Both schemes use __Host- names, which a sibling site cannot set.
	for _, got := range []*http.Cookie{sessionCookie("v", 60), loginCookie(loginStateCookieName, "v", 60)} {
		if !strings.HasPrefix(got.Name, "__Host-") || !got.Secure || got.Path != "/" || got.Domain != "" ||
			!got.Partitioned || got.SameSite != http.SameSiteNoneMode {
			t.Fatalf("login cookie = %+v", got)
		}
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

	// An open site's page starts the sign-in itself through /api/auth/login.
	rec := st.do(navigation(siteRequest(http.MethodGet, host, "/api/auth/login?return_to=%2Fapp%3Fx%3D1")))
	location, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || location.Host != "chat.example.com" ||
		location.Query().Get("return_to") != "http://"+host+"/app?x=1" {
		t.Fatalf("sign-in start = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	state := responseCookie(t, rec, loginStateCookieName)
	if location.Query().Get("state") != state.Value {
		t.Fatalf("sign-in state = %q, cookie %q", location.Query().Get("state"), state.Value)
	}
	claims := testLoginClaims(host, "viewer", time.Now())
	claims["email"] = "viewer@example.com"
	claims["state"] = state.Value
	callback := siteRequest(http.MethodGet, host, "/api/auth/callback?token="+signTestLoginToken(t, testLoginTokenSecret, nil, claims)+"&return_to=%2Fapp")
	callback.AddCookie(&http.Cookie{Name: state.Name, Value: state.Value})
	rec = st.do(callback)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback after sign-in start = %d %s", rec.Code, rec.Body.String())
	}
	cookie := responseCookie(t, rec, sessionCookieName)

	// The sign-in start never leaves the site host.
	rec = st.do(navigation(siteRequest(http.MethodGet, host, "/api/auth/login?return_to=%2F%2Fevil.example")))
	if location, err := url.Parse(rec.Header().Get("Location")); err != nil || location.Query().Get("return_to") != "http://"+host+"/" {
		t.Fatalf("sign-in start with foreign return_to = %q", rec.Header().Get("Location"))
	}
	if rec := st.do(navigation(siteRequest(http.MethodGet, "sites.localhost:8443", "/api/auth/login"))); rec.Code != http.StatusNotFound {
		t.Fatalf("apex sign-in start = %d, want 404", rec.Code)
	}

	req := siteRequest(http.MethodGet, host, "/api/db/posts")
	req.AddCookie(cookie)
	if rec := st.do(req); rec.Code != http.StatusOK {
		t.Fatalf("signed-in db list = %d %s", rec.Code, rec.Body.String())
	}
	req = siteRequest(http.MethodGet, host, "/api/me")
	req.AddCookie(cookie)
	rec = st.do(req)
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

	// Denied: the same question through the public proxy, on a site host or
	// the apex, so outsiders cannot list site names.
	for _, host := range []string{"live.sites.localhost:8443", "sites.localhost:8443"} {
		if rec := st.do(siteRequest(http.MethodGet, host, "/api/tls/ask?domain=live.sites.localhost")); rec.Code != http.StatusNotFound {
			t.Fatalf("tls ask via %s = %d, want 404", host, rec.Code)
		}
	}

	// Denied: a proxied request naming the internal host, as a client can by
	// pairing a valid site SNI with Host: sites:8080.
	proxied := siteRequest(http.MethodGet, "sites:8080", "/api/tls/ask?domain=live.sites.localhost")
	if rec := st.do(proxied); rec.Code != http.StatusNotFound {
		t.Fatalf("tls ask with forwarded internal host = %d, want 404", rec.Code)
	}

	// Not rate-limited: Caddy asks from one address for every unknown name.
	st.srv.dbLimit = NewRateLimiter(1, 1)
	st.handler = st.srv.routes()
	for i := 0; i < 3; i++ {
		if rec := st.do(httptest.NewRequest(http.MethodGet, "http://sites:8080/api/tls/ask?domain=live.sites.localhost", nil)); rec.Code != http.StatusOK {
			t.Fatalf("tls ask %d = %d, want 200", i, rec.Code)
		}
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

// A request carrying two session cookies must not pick either identity, and
// an unprefixed spot_session, which a sibling site can set with a parent
// Domain, carries none.
func TestDelegatedLoginRejectsDuplicateSessionCookies(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com","mallory@example.com"]}`)
	owner := st.signIn(t, host, "owner@example.com", "owner-dup")
	planted := st.signIn(t, host, "mallory@example.com", "mallory-dup")

	withCookies := func(req *http.Request, cookies ...*http.Cookie) *http.Request {
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		return req
	}

	// Allowed: exactly one valid session cookie.
	if rec := st.do(withCookies(siteRequest(http.MethodGet, host, "/api/me"), owner)); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"email":"owner@example.com"`) {
		t.Fatalf("single cookie /api/me = %d %s", rec.Code, rec.Body.String())
	}

	// Denied: two cookies, in either order, are anonymous everywhere.
	for _, order := range [][]*http.Cookie{{owner, planted}, {planted, owner}, {owner, owner}} {
		rec := st.do(withCookies(siteRequest(http.MethodGet, host, "/api/me"), order...))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "more than one Spot session cookie") {
			t.Fatalf("duplicate cookies /api/me = %d %s, want 401", rec.Code, rec.Body.String())
		}
		rec = st.do(withCookies(siteRequest(http.MethodGet, host, "/index.html"), order...))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "more than one Spot session cookie") {
			t.Fatalf("duplicate cookies fetch = %d %s, want 401", rec.Code, rec.Body.String())
		}
		rec = st.do(withCookies(navigation(siteRequest(http.MethodGet, host, "/")), order...))
		if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://chat.example.com/sites/login?") {
			t.Fatalf("duplicate cookies navigation = %d %q, want login redirect", rec.Code, rec.Header().Get("Location"))
		}
		// The check page ends the login loop with an explanation.
		rec = st.do(withCookies(siteRequest(http.MethodGet, host, "/api/auth/check?return_to=%2F"), order...))
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" ||
			!strings.Contains(rec.Body.String(), "more than one session cookie") {
			t.Fatalf("duplicate cookies check = %d %q %s, want 400", rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}

	// A planted unprefixed cookie neither counts as a duplicate nor signs in.
	unprefixed := &http.Cookie{Name: "spot_session", Value: planted.Value}
	if rec := st.do(withCookies(siteRequest(http.MethodGet, host, "/api/me"), owner, unprefixed)); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"email":"owner@example.com"`) {
		t.Fatalf("session plus unprefixed cookie = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(withCookies(siteRequest(http.MethodGet, host, "/api/me"), unprefixed)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unprefixed cookie alone = %d %s, want 401", rec.Code, rec.Body.String())
	}
}

// Session cookies exist only for site hosts; on the apex the platform APIs
// must ignore them, even one signed for the apex host itself.
func TestSiteSessionConfersNothingOnApex(t *testing.T) {
	st := newLoginTestStack(t)
	const apex = "sites.localhost:8443"
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"]}`)
	siteSession := st.signIn(t, "demo.sites.localhost:8443", "owner@example.com", "owner-apex")
	apexValue, err := st.srv.login.signSession(Identity{Email: "owner@example.com", Groups: []string{}}, apex)
	if err != nil {
		t.Fatal(err)
	}
	apexSession := &http.Cookie{Name: sessionCookieName, Value: apexValue}

	for _, cookie := range []*http.Cookie{siteSession, apexSession} {
		mine := siteRequest(http.MethodGet, apex, "/api/sites/mine")
		mine.AddCookie(cookie)
		if rec := st.do(mine); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no identity") {
			t.Fatalf("apex /api/sites/mine with session = %d %s, want 404 no identity", rec.Code, rec.Body.String())
		}
		access := siteRequestWithBody(http.MethodPut, apex, "/api/sites/demo/access", `{}`)
		access.AddCookie(cookie)
		if rec := st.do(access); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no identity") {
			t.Fatalf("apex access change with session = %d %s, want 404 no identity", rec.Code, rec.Body.String())
		}
	}
	if _, found, err := st.srv.resolvePeer(func() *http.Request {
		req := siteRequest(http.MethodGet, apex, "/")
		req.AddCookie(apexSession)
		return req
	}()); err != nil || found {
		t.Fatalf("apex session resolved = %v, %v", found, err)
	}

	// Allowed: the platform identity on the apex, and the session on its host.
	if rec := st.do(asForwardUser(sitesRequestFor("/api/sites/mine"), "owner@example.com")); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"name":"demo"`) {
		t.Fatalf("apex /api/sites/mine with forward auth = %d %s", rec.Code, rec.Body.String())
	}
	page := siteRequest(http.MethodGet, "demo.sites.localhost:8443", "/")
	page.AddCookie(siteSession)
	if rec := st.do(page); rec.Code != http.StatusOK {
		t.Fatalf("site page with its session = %d", rec.Code)
	}
	// The access change above did nothing.
	if rec := st.do(siteRequest(http.MethodGet, "demo.sites.localhost:8443", "/")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private site anonymous after rejected apex change = %d, want 401", rec.Code)
	}
}

func TestIsNavigation(t *testing.T) {
	for _, tt := range []struct {
		name, method, mode, accept string
		want                       bool
	}{
		{"navigate mode", http.MethodGet, "navigate", "", true},
		{"accept html only", http.MethodGet, "", "text/html,application/xhtml+xml", true},
		{"head navigate", http.MethodHead, "navigate", "", true},
		{"head accept html", http.MethodHead, "", "text/html", true},
		{"fetch json", http.MethodGet, "cors", "application/json", false},
		{"no hints", http.MethodGet, "", "", false},
		{"post navigate", http.MethodPost, "navigate", "text/html", false},
	} {
		req := httptest.NewRequest(tt.method, "http://spot-api/", nil)
		if tt.mode != "" {
			req.Header.Set("Sec-Fetch-Mode", tt.mode)
		}
		if tt.accept != "" {
			req.Header.Set("Accept", tt.accept)
		}
		if got := isNavigation(req); got != tt.want {
			t.Errorf("%s: isNavigation = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDelegatedLoginRedirectsAcceptOnlyAndHeadNavigations(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	isLoginRedirect := func(rec *httptest.ResponseRecorder) bool {
		return rec.Code == http.StatusFound && strings.HasPrefix(rec.Header().Get("Location"), "https://chat.example.com/sites/login?")
	}

	acceptOnly := siteRequest(http.MethodGet, host, "/")
	acceptOnly.Header.Set("Accept", "text/html,*/*")
	if rec := st.do(acceptOnly); !isLoginRedirect(rec) {
		t.Fatalf("Accept-only navigation = %d %q, want login redirect", rec.Code, rec.Header().Get("Location"))
	}
	head := siteRequest(http.MethodHead, host, "/")
	head.Header.Set("Sec-Fetch-Mode", "navigate")
	if rec := st.do(head); !isLoginRedirect(rec) {
		t.Fatalf("HEAD navigation = %d %q, want login redirect", rec.Code, rec.Header().Get("Location"))
	}
	fetch := siteRequest(http.MethodGet, host, "/")
	fetch.Header.Set("Accept", "application/json")
	fetch.Header.Set("Sec-Fetch-Mode", "cors")
	if rec := st.do(fetch); rec.Code != http.StatusUnauthorized {
		t.Fatalf("script fetch = %d, want 401", rec.Code)
	}
}

func TestDelegatedLoginDeniedNavigationGetsStatusPage(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"]}`)
	host := "demo.sites.localhost:8443"
	cookie := st.signIn(t, host, "stranger@example.com", "denied-page-jti")

	rec := st.do(withCookie(navigation(siteRequest(http.MethodGet, host, "/")), cookie))
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "You don&#39;t have access to this site") || !strings.Contains(body, "Signed in as stranger@example.com") ||
		strings.Contains(body, "owner@example.com") {
		t.Fatalf("denied navigation = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), body)
	}

	rec = st.do(withCookie(siteRequest(http.MethodGet, host, "/"), cookie))
	if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("denied API call = %d %q, want JSON 403", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func withCookie(req *http.Request, cookie *http.Cookie) *http.Request {
	req.AddCookie(cookie)
	return req
}

// Login CSRF: a token minted for the attacker's own account must not sign in
// a browser that did not start that sign-in.
func TestDelegatedLoginCallbackIsBoundToTheBrowser(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com","mallory@example.com"]}`)
	victim := st.signIn(t, host, "owner@example.com", "victim-login")

	claims := testLoginClaims(host, "mallory-login", time.Now())
	claims["email"] = "mallory@example.com"
	claims["state"] = otherLoginState
	attackerLink := "/api/auth/callback?token=" + signTestLoginToken(t, testLoginTokenSecret, nil, claims) + "&return_to=%2F"

	// Denied: a browser with no pending sign-in gets no session; a signed-in
	// victim keeps their own and continues to the check page.
	rec := st.do(siteRequest(http.MethodGet, host, attackerLink))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Open this site in a new tab") ||
		len(rec.Result().Cookies()) != 0 {
		t.Fatalf("callback without login state = %d %v", rec.Code, rec.Result().Cookies())
	}
	rec = st.do(withCookie(siteRequest(http.MethodGet, host, attackerLink), victim))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/api/auth/check?return_to=%2F" ||
		len(rec.Result().Cookies()) != 0 {
		t.Fatalf("callback without login state, signed in = %d %q %v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
	}
	// Denied: a victim mid-sign-in holds a different state.
	rec = st.do(withLoginState(siteRequest(http.MethodGet, host, attackerLink)))
	if rec.Code != http.StatusBadRequest || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("callback with another browser's state = %d %v", rec.Code, rec.Result().Cookies())
	}
	// Denied: conflicting state cookies.
	dup := withLoginState(siteRequest(http.MethodGet, host, attackerLink))
	dup.AddCookie(&http.Cookie{Name: loginStateCookieName, Value: otherLoginState})
	if rec := st.do(dup); rec.Code != http.StatusBadRequest || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("callback with duplicate login states = %d %v", rec.Code, rec.Result().Cookies())
	}
	if rec := st.do(withCookie(siteRequest(http.MethodGet, host, "/api/me"), victim)); !strings.Contains(rec.Body.String(), `"email":"owner@example.com"`) {
		t.Fatalf("victim identity after attack = %s", rec.Body.String())
	}

	// Allowed: the browser that holds the matching state; rejected attempts
	// did not burn the token.
	own := siteRequest(http.MethodGet, host, attackerLink)
	own.AddCookie(&http.Cookie{Name: loginStateCookieName, Value: otherLoginState})
	if rec := st.do(own); rec.Code != http.StatusFound {
		t.Fatalf("callback with matching state = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDelegatedLoginReusesPendingState(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)

	rec := st.do(withLoginState(navigation(siteRequest(http.MethodGet, host, "/"))))
	if got := responseCookie(t, rec, loginStateCookieName).Value; got != testLoginState {
		t.Fatalf("second tab state = %q, want the pending %q", got, testLoginState)
	}
	malformed := navigation(siteRequest(http.MethodGet, host, "/"))
	malformed.AddCookie(&http.Cookie{Name: loginStateCookieName, Value: "short"})
	rec = st.do(malformed)
	if got := responseCookie(t, rec, loginStateCookieName).Value; got == "short" || len(got) != 43 {
		t.Fatalf("state after malformed cookie = %q, want a fresh one", got)
	}
}

// Off HTTPS and *.localhost a sibling site can plant cookies, so delegated
// login neither issues nor honors sessions there.
func TestDelegatedLoginRequiresSecureTransport(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "private.sites.example.com"
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	st.srv.spotDomain = "sites.example.com"

	rec := st.do(navigation(siteRequest(http.MethodGet, host, "/")))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "secure connection") || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("plain http navigation = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(navigation(siteRequest(http.MethodGet, host, "/api/auth/login"))); rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("plain http sign-in start = %d %v", rec.Code, rec.Result().Cookies())
	}
	token := signTestLoginToken(t, testLoginTokenSecret, nil, testLoginClaims(host, "plain", time.Now()))
	rec = st.do(withLoginState(siteRequest(http.MethodGet, host, "/api/auth/callback?token="+token)))
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("plain http callback = %d %v", rec.Code, rec.Result().Cookies())
	}
	value, err := st.srv.login.signSession(Identity{Email: "owner@example.com", Groups: []string{}}, host)
	if err != nil {
		t.Fatal(err)
	}
	page := siteRequest(http.MethodGet, host, "/")
	page.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	if rec := st.do(page); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain http session = %d, want 401", rec.Code)
	}
	check := siteRequest(http.MethodGet, host, "/api/auth/check")
	check.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	if rec := st.do(check); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "secure connection") {
		t.Fatalf("plain http check = %d %s, want the secure-connection page", rec.Code, rec.Body.String())
	}

	// Allowed: the same session over HTTPS.
	secure := siteRequest(http.MethodGet, host, "/")
	secure.Header.Set("X-Forwarded-Proto", "https")
	secure.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	if rec := st.do(secure); rec.Code != http.StatusOK {
		t.Fatalf("https session = %d %s", rec.Code, rec.Body.String())
	}
}

type countingResolver struct {
	id    Identity
	calls int
}

func (c *countingResolver) Resolve(context.Context, string) (Identity, bool, error) {
	c.calls++
	return c.id, true, nil
}

// requireVisitor hands its identity to the handler instead of asking the mesh
// resolver a second time.
func TestRequireVisitorResolvesOnce(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open", "")
	resolver := &countingResolver{id: Identity{Email: "mesh@example.com", Groups: []string{}}}
	st.srv.resolver = resolver

	rec := st.do(siteRequest(http.MethodGet, "open.sites.localhost:8443", "/api/me"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"mesh@example.com"`) {
		t.Fatalf("mesh /api/me = %d %s", rec.Code, rec.Body.String())
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}

// The apex has no viewer sessions, so a page load there never starts a
// sign-in: it would set cookies on the apex and end at a callback that 404s.
func TestApexNavigationDoesNotStartSignIn(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	upload, err := st.srv.files.Put(context.Background(), "private", "a.txt", "text/plain", strings.NewReader("a"), 1)
	if err != nil {
		t.Fatal(err)
	}
	rec := st.do(navigation(siteRequest(http.MethodGet, "sites.localhost:8443", upload.URL)))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
		t.Fatalf("apex navigation = %d %q %v, want JSON 401", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
	}
	// Allowed: on the site host, opening the upload link, or any page, starts
	// a sign-in.
	rec = st.do(navigation(siteRequest(http.MethodGet, "private.sites.localhost:8443", upload.URL)))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://chat.example.com/sites/login?") {
		t.Fatalf("site host upload link = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = st.do(navigation(siteRequest(http.MethodGet, "private.sites.localhost:8443", "/")))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://chat.example.com/sites/login?") {
		t.Fatalf("site host navigation = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A realtime socket opened with a delegated session must close when the
// session expires; otherwise it outlives SPOT_SESSION_TTL indefinitely.
func TestDelegatedSessionExpiryClosesWebSocket(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "open.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "open", "")
	st.srv.login.sessionTTL = 2 * time.Second
	value, err := st.srv.login.signSession(Identity{Email: "viewer@example.com", Groups: []string{}}, host)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(st.handler)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Forwarded-Host": []string{host},
			"Cookie":           []string{sessionCookieName + "=" + value},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, wsRequest{Type: "subscribe", Collection: "posts"}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]string
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack["type"] != "subscribed" {
		t.Fatalf("subscribe ack = %v, %v", ack, err)
	}

	var message any
	err = wsjson.Read(ctx, conn, &message)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("websocket after session expiry: read %#v, %v; want it closed", message, err)
	}
}
