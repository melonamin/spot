package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPublishingKeyTokenRoundTripAllowsBase64URLUnderscore(t *testing.T) {
	id := strings.Repeat("ab", publishingKeyIDBytes)
	secret := bytes.Repeat([]byte{0xff}, publishingKeySecretBytes)
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	if !strings.Contains(encoded, "_") {
		t.Fatal("test secret does not exercise underscore parsing")
	}
	gotID, gotSecret, err := parsePublishingKeyToken(publishingKeyTokenPrefix + id + "_" + encoded)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id || !bytes.Equal(gotSecret, secret) {
		t.Fatalf("parsed token = %q/%x", gotID, gotSecret)
	}
}

func TestPublishingKeyStoreLifecycle(t *testing.T) {
	db := openTestDB(t)
	store := NewPublishingKeyStore(db)
	actor := Identity{Email: "Alice@Example.com", Name: "Alice", PeerIP: "100.64.0.8"}
	key, token, err := store.Create(context.Background(), actor, "  GitHub Actions · repo  ", "repo-pr-")
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "GitHub Actions · repo" || key.OwnerEmail != "alice@example.com" || key.OwnerPeerIP != "" {
		t.Fatalf("created key = %+v", key)
	}
	if strings.Contains(token, key.Name) || !strings.HasPrefix(token, publishingKeyTokenPrefix+key.ID+"_") {
		t.Fatalf("token format = %q", token)
	}
	authenticated, err := store.Authenticate(context.Background(), token)
	if err != nil || authenticated.ID != key.ID {
		t.Fatalf("authenticate = %+v, %v", authenticated, err)
	}
	listed, err := store.List(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil || len(listed) != 1 || listed[0].ID != key.ID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	foreign, err := store.List(context.Background(), Identity{Email: "bob@example.com"})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign list = %+v, %v", foreign, err)
	}
	if _, _, err := store.Create(context.Background(), actor, "CI rotation", "repo-pr-"); err != nil {
		t.Fatalf("overlapping rotation key: %v", err)
	}
	listed, _ = store.List(context.Background(), actor)
	if len(listed) != 2 {
		t.Fatalf("overlapping keys = %d, want 2", len(listed))
	}
	if err := store.TouchUsed(context.Background(), key.ID); err != nil {
		t.Fatal(err)
	}
	listed, _ = store.List(context.Background(), actor)
	var used bool
	for _, candidate := range listed {
		if candidate.ID == key.ID && candidate.LastUsedAt != nil {
			used = true
		}
	}
	if !used {
		t.Fatal("last_used_at was not recorded")
	}
	if err := store.Revoke(context.Background(), key.ID, Identity{Email: "bob@example.com"}, false); err != errPublishingKeyNotFound {
		t.Fatalf("foreign revoke error = %v", err)
	}
	if err := store.Revoke(context.Background(), key.ID, actor, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(context.Background(), token); err != errPublishingKeyInvalid {
		t.Fatalf("revoked authenticate error = %v", err)
	}
	var storedHash []byte
	if err := db.QueryRow(`SELECT secret_hash FROM publishing_keys WHERE id = ?`, key.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != nil {
		t.Fatalf("revoked hash = %x, want nil", storedHash)
	}
	listed, _ = store.List(context.Background(), actor)
	var revoked bool
	for _, candidate := range listed {
		if candidate.ID == key.ID && candidate.RevokedAt != nil {
			revoked = true
		}
	}
	if !revoked {
		t.Fatal("revoked_at was not recorded")
	}
}

func TestPublishingKeyListIncludesEarlierPeerOwnershipAfterEmailResolution(t *testing.T) {
	db := openTestDB(t)
	store := NewPublishingKeyStore(db)
	key, _, err := store.Create(context.Background(), Identity{PeerIP: "100.64.0.7", Name: "Peer"}, "Peer CI", "peer-pr-")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(context.Background(), Identity{Email: "alice@example.com", PeerIP: "100.64.0.7"})
	if err != nil || len(listed) != 1 || listed[0].ID != key.ID {
		t.Fatalf("enriched peer list = %+v, %v", listed, err)
	}
}

func TestPublishingKeyStoreValidatesInputAndEntropyFailure(t *testing.T) {
	db := openTestDB(t)
	store := NewPublishingKeyStore(db)
	actor := Identity{Email: "alice@example.com"}
	for _, prefix := range []string{"", "repo", "Repo-", "-repo-", strings.Repeat("a", 61) + "-"} {
		if _, _, err := store.Create(context.Background(), actor, "CI", prefix); err == nil {
			t.Errorf("prefix %q accepted", prefix)
		}
	}
	store.rand = strings.NewReader("short")
	if _, _, err := store.Create(context.Background(), actor, "CI", "repo-"); err == nil {
		t.Fatal("short entropy source accepted")
	}
}

func TestPublishingKeyAuthenticateUsesSecretHash(t *testing.T) {
	db := openTestDB(t)
	store := NewPublishingKeyStore(db)
	key, token, err := store.Create(context.Background(), Identity{Email: "alice@example.com"}, "CI", "repo-")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := parsePublishingKeyToken(token)
	hash := sha256.Sum256(secret)
	var stored []byte
	if err := db.QueryRow(`SELECT secret_hash FROM publishing_keys WHERE id = ?`, key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, hash[:]) || bytes.Contains(stored, secret) {
		t.Fatalf("stored verifier = %x", stored)
	}
	secretStart := len(publishingKeyTokenPrefix) + len(key.ID) + 1
	replacement := byte('A')
	if token[secretStart] == replacement {
		replacement = 'B'
	}
	bad := token[:secretStart] + string(replacement) + token[secretStart+1:]
	if _, err := store.Authenticate(context.Background(), bad); err != errPublishingKeyInvalid {
		t.Fatalf("bad secret error = %v", err)
	}
}

func TestPublishingKeyOperationalAuthenticationFailureIs503(t *testing.T) {
	db := openTestDB(t)
	store := NewPublishingKeyStore(db)
	_, token, err := store.Create(context.Background(), Identity{Email: "alice@example.com"}, "CI", "repo-")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &Server{publishingKeys: store}
	req := httptest.NewRequest(http.MethodPost, "http://spot.localhost/api/deploy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	if _, ok := srv.requireDeployPrincipal(rec, req); ok {
		t.Fatal("closed database authenticated publishing key")
	}
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "sql") {
		t.Fatalf("operational authentication failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublishingKeyPolicyTransitionClearRevalidatesOwnerAndRevocation(t *testing.T) {
	db := openTestDB(t)
	keys := NewPublishingKeyStore(db)
	registry := NewSiteRegistry(db, &AccessPolicy{Allow: []string{"alice@example.com"}})
	actor := Identity{Email: "alice@example.com"}
	key, _, err := keys.Create(context.Background(), actor, "CI", "repo-pr-")
	if err != nil {
		t.Fatal(err)
	}
	for site, owner := range map[string]string{
		"repo-pr-owned":   "alice@example.com",
		"repo-pr-foreign": "bob@example.com",
		"repo-pr-revoked": "alice@example.com",
		"repo-pr-stale":   "alice@example.com",
	} {
		if _, err := db.Exec(`INSERT INTO sites (name, owner_email, content_generation) VALUES (?, ?, 1)`, site, owner); err != nil {
			t.Fatal(err)
		}
		if err := registry.BeginPolicyTransition(context.Background(), site, 1, absentPolicyHash, absentPolicyHash); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.ClearPolicyTransitionForPublishingKey(context.Background(), "repo-pr-foreign", 1, actor, key.ID); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("foreign fence clear = %v", err)
	}
	if pending, err := registry.HasPendingPolicyTransition(context.Background(), "repo-pr-foreign"); err != nil || !pending {
		t.Fatalf("foreign fence pending = %v, %v", pending, err)
	}
	if err := registry.ClearPolicyTransitionForPublishingKey(context.Background(), "repo-pr-owned", 1, actor, key.ID); err != nil {
		t.Fatalf("owned fence clear = %v", err)
	}
	if _, err := db.Exec(`UPDATE sites SET content_generation = 2 WHERE name = 'repo-pr-stale'`); err != nil {
		t.Fatal(err)
	}
	if err := registry.ClearPolicyTransitionForPublishingKey(context.Background(), "repo-pr-stale", 1, actor, key.ID); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("stale-generation fence clear = %v", err)
	}
	if pending, err := registry.HasPendingPolicyTransition(context.Background(), "repo-pr-stale"); err != nil || !pending {
		t.Fatalf("stale-generation fence pending = %v, %v", pending, err)
	}
	if err := keys.Revoke(context.Background(), key.ID, actor, false); err != nil {
		t.Fatal(err)
	}
	if err := registry.ClearPolicyTransitionForPublishingKey(context.Background(), "repo-pr-revoked", 1, actor, key.ID); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("revoked fence clear = %v", err)
	}
	if pending, err := registry.HasPendingPolicyTransition(context.Background(), "repo-pr-revoked"); err != nil || !pending {
		t.Fatalf("revoked fence pending = %v, %v", pending, err)
	}
}

func TestPublishingKeyManagementAPI(t *testing.T) {
	db := openTestDB(t)
	srv := &Server{
		publishingKeys: NewPublishingKeyStore(db),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}

	create := httptest.NewRequest(http.MethodPost, "http://spot.localhost/api/publishing-keys",
		strings.NewReader(`{"name":"GitHub Actions · repo","site_prefix":"repo-pr-"}`))
	create.Header.Set("Content-Type", "application/json")
	create.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create = %d cache=%q body=%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	var created struct {
		Key    PublishingKey `json:"key"`
		Secret string        `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" || created.Key.ID == "" {
		t.Fatalf("create response = %+v", created)
	}

	list := httptest.NewRequest(http.MethodGet, "http://spot.localhost/api/publishing-keys", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, list)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), created.Secret) || strings.Contains(rec.Body.String(), "secret_hash") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	revoke := httptest.NewRequest(http.MethodDelete, "http://spot.localhost/api/publishing-keys/"+created.Key.ID, nil)
	revoke.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, revoke)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPublishingKeyDeployEndToEnd(t *testing.T) {
	db := openTestDB(t)
	keys := NewPublishingKeyStore(db)
	registry := NewSiteRegistry(db, &AccessPolicy{Allow: []string{"alice@example.com"}})
	sites := newTestSiteStore(t)
	srv := &Server{
		publishingKeys: keys,
		deployAuth:     registry,
		siteAdmin:      registry,
		sites:          sites,
		policies:       NewPolicyStore("", 0),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	registry.SetPolicyResolver(srv.policyForSite)
	key, token, err := keys.Create(context.Background(), Identity{Email: "alice@example.com", Name: "Alice"}, "GitHub Actions · repo", "repo-pr-")
	if err != nil {
		t.Fatal(err)
	}

	deploy := func(site, bodyToken string) *httptest.ResponseRecorder {
		req := deployRequest(t, "spot.localhost", site, map[string]string{
			"index.html":   "<h1>preview</h1>",
			accessFileName: `{"maintainers":["team-preview"]}`,
		})
		req.Host = "spot.localhost"
		req.Header.Del("X-Forwarded-Host")
		req.Header.Set("Authorization", "Bearer "+bodyToken)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec
	}
	var securityLog bytes.Buffer
	oldLogOutput, oldLogFlags := log.Writer(), log.Flags()
	log.SetOutput(&securityLog)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldLogOutput)
		log.SetFlags(oldLogFlags)
	})

	if rec := deploy("repo-pr-17", token); rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var ownerEmail string
	if err := db.QueryRow(`SELECT owner_email FROM sites WHERE name = 'repo-pr-17'`).Scan(&ownerEmail); err != nil || ownerEmail != "alice@example.com" {
		t.Fatalf("owner = %q, %v", ownerEmail, err)
	}
	if rec := deploy("repo-pr-17", token); rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if rec := deploy("outside-17", token); rec.Code != http.StatusForbidden {
		t.Fatalf("outside prefix = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := db.Exec(`INSERT INTO sites (name, owner_email, content_generation) VALUES ('repo-pr-foreign', 'bob@example.com', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginPolicyTransition(context.Background(), "repo-pr-foreign", 1, absentPolicyHash, absentPolicyHash); err != nil {
		t.Fatal(err)
	}
	if rec := deploy("repo-pr-foreign", token); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fenced foreign site = %d %s", rec.Code, rec.Body.String())
	}
	if pending, err := registry.HasPendingPolicyTransition(context.Background(), "repo-pr-foreign"); err != nil || !pending {
		t.Fatalf("foreign deploy cleared policy fence = %v, %v", pending, err)
	}
	var method, keyID, publisher string
	if err := db.QueryRow(`SELECT auth_method, publisher_key_id, publisher_name
		FROM site_deploy_audit WHERE site = 'repo-pr-17' AND status = 'success'
		ORDER BY id DESC LIMIT 1`).Scan(&method, &keyID, &publisher); err != nil {
		t.Fatal(err)
	}
	if method != "publishing_key" || keyID != key.ID || publisher != "GitHub Actions · repo" {
		t.Fatalf("audit = %q %q %q", method, keyID, publisher)
	}
	manageableReq := httptest.NewRequest(http.MethodGet, "http://spot.localhost/api/sites/manageable", nil)
	manageableRec := httptest.NewRecorder()
	srv.handleManageableSites(manageableRec, manageableReq)
	if manageableRec.Code != http.StatusOK || !strings.Contains(manageableRec.Body.String(), `"publisher_name":"GitHub Actions · repo"`) {
		t.Fatalf("manageable attribution = %d %s", manageableRec.Code, manageableRec.Body.String())
	}
	publicReq := httptest.NewRequest(http.MethodGet, "http://spot.localhost/api/sites/public", nil)
	publicRec := httptest.NewRecorder()
	srv.handlePublicSites(publicRec, publicReq)
	if strings.Contains(publicRec.Body.String(), "publisher_name") || strings.Contains(publicRec.Body.String(), "GitHub Actions") {
		t.Fatalf("public sites leaked publisher attribution: %s", publicRec.Body.String())
	}
	listed, _ := keys.List(context.Background(), Identity{Email: "alice@example.com"})
	if listed[0].LastUsedAt == nil {
		t.Fatal("successful deploy did not update last_used_at")
	}
	if err := keys.Revoke(context.Background(), key.ID, Identity{Email: "alice@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	if rec := deploy("repo-pr-18", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked = %d %s", rec.Code, rec.Body.String())
	}
	if rec := deploy("repo-pr-18", token[:len(token)-1]+"A"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid with ambient identity = %d %s", rec.Code, rec.Body.String())
	}
	unknown := publishingKeyTokenPrefix + strings.Repeat("0", publishingKeyIDBytes*2) + token[len(publishingKeyTokenPrefix)+publishingKeyIDBytes*2:]
	if rec := deploy("repo-pr-18", unknown); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d %s", rec.Code, rec.Body.String())
	}
	if rec := deploy("repo-pr-18", "malformed"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed key = %d %s", rec.Code, rec.Body.String())
	}
	logs := securityLog.String()
	if !strings.Contains(logs, "id="+key.ID+" site=outside-17") ||
		!strings.Contains(logs, "authentication rejected: id="+key.ID) ||
		!strings.Contains(logs, "authentication rejected: id="+strings.Repeat("0", publishingKeyIDBytes*2)) ||
		!strings.Contains(logs, "publishing key authentication rejected") {
		t.Fatalf("security logs missing bounded rejection context: %s", logs)
	}
	if strings.Contains(logs, token) {
		t.Fatal("security logs exposed publishing key token")
	}
}

func TestAmbientDeployAuditUsesConfiguredMethod(t *testing.T) {
	for _, method := range []string{"dev", "single_user"} {
		t.Run(method, func(t *testing.T) {
			db := openTestDB(t)
			registry := NewSiteRegistry(db, nil)
			srv := &Server{
				deployAuth: registry, deployAuthMethod: method,
				sites: newTestSiteStore(t), resolver: NewStaticResolver("alice@example.com", "Alice", nil),
				spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t),
				deployLimit: NewRateLimiter(1000, 1000),
			}
			req := deployRequest(t, "spot.localhost", "audit-"+strings.ReplaceAll(method, "_", "-"), map[string]string{"index.html": "ok"})
			req.Host = "spot.localhost"
			req.Header.Del("X-Forwarded-Host")
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("deploy = %d %s", rec.Code, rec.Body.String())
			}
			var got string
			if err := db.QueryRow(`SELECT auth_method FROM site_deploy_audit WHERE site = ? AND status = 'success'`, "audit-"+strings.ReplaceAll(method, "_", "-")).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != method {
				t.Fatalf("auth method = %q, want %q", got, method)
			}
		})
	}
}

func TestPublishingKeysUIContract(t *testing.T) {
	raw, err := os.ReadFile("../sdk/spots.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, marker := range []string{
		`id="automation-title"`,
		`id="key-manager-open"`,
		`id="key-manager-panel"`,
		`id="key-dialog"`,
		`id="key-secret"`,
		`/api/publishing-keys`,
		`cache: 'no-store'`,
		`Repository publishing keys`,
		`id="key-overlap"`,
		`already covers an overlapping prefix`,
		`let keyCreating = false`,
		`if (keyCreating) event.preventDefault()`,
		`site.last_deploy.publisher_name`,
		`key-secret').textContent = ''`,
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("spots UI missing %q", marker)
		}
	}
}
