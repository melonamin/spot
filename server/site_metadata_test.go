package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSiteMetadataFileNormalizesTags(t *testing.T) {
	meta, err := parseSiteMetadataFile([]byte(`{
		"title":" Demo App ",
		"description":"A tiny demo",
		"tags":["AI Tools","dashboard","ai_tools","dashboard",""]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.TagsSpecified {
		t.Fatal("TagsSpecified = false, want true")
	}
	want := []string{"ai-tools", "dashboard", "ai-tools"}
	if len(meta.Tags) != 2 || meta.Tags[0] != want[0] || meta.Tags[1] != want[1] {
		t.Fatalf("tags = %v, want [ai-tools dashboard]", meta.Tags)
	}
	if meta.Title != "Demo App" || meta.Description != "A tiny demo" {
		t.Fatalf("metadata = %+v", meta)
	}
}

func TestParseSiteMetadataFileRejectsBadTags(t *testing.T) {
	_, err := parseSiteMetadataFile([]byte(`{"tags":["bad/tag"]}`))
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("parse bad tag err = %v, want tag error", err)
	}
}

func TestParseSiteMetadataFileNullTagsAreExplicitEmpty(t *testing.T) {
	meta, err := parseSiteMetadataFile([]byte(`{"tags":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.TagsSpecified {
		t.Fatal("TagsSpecified = false, want true")
	}
	if len(meta.Tags) != 0 {
		t.Fatalf("tags = %v, want empty", meta.Tags)
	}
}

func TestParseSiteMetadataFileRejectsTrailingTokens(t *testing.T) {
	_, err := parseSiteMetadataFile([]byte(`{"title":"Demo"} {}`))
	if err == nil || !strings.Contains(err.Error(), "single JSON object") {
		t.Fatalf("parse trailing tokens err = %v, want single object error", err)
	}
}

func TestMetadataFromIndexHTMLKeepsQuotedDescriptionText(t *testing.T) {
	meta := metadataFromIndexHTML([]byte(`<title>Quotes</title><meta name="description" content="Bob's game &amp; Alice's tools">`))
	if meta.Description != "Bob's game & Alice's tools" {
		t.Fatalf("description = %q, want apostrophes preserved", meta.Description)
	}

	meta = metadataFromIndexHTML([]byte(`<meta content='A "quoted" dashboard' property='og:description'>`))
	if meta.Description != `A "quoted" dashboard` {
		t.Fatalf("reversed description = %q, want double quotes preserved", meta.Description)
	}
}

func TestDeployStoresManualSiteMetadata(t *testing.T) {
	srv, db := newMetadataDeployServer(t, nil)
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "demo", map[string]string{
		"index.html": `<title>Fallback</title><h1>Hello</h1>`,
		"_spot.json": `{"title":"Manual title","description":"Manual description","tags":["tools","demo"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/public"))
	if rec.Code != http.StatusOK {
		t.Fatalf("public sites = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []publicSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 {
		t.Fatalf("sites = %+v, want one", body.Sites)
	}
	site := body.Sites[0]
	if site.Title != "Manual title" || site.Description != "Manual description" {
		t.Fatalf("metadata = %+v", site)
	}
	if len(site.Tags) != 2 || site.Tags[0] != "tools" || site.Tags[1] != "demo" {
		t.Fatalf("tags = %v, want [tools demo]", site.Tags)
	}
}

func TestDeployAutoTagsPublicSiteWithAI(t *testing.T) {
	aiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected AI path %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model": "tagger",
			"choices": []map[string]any{{
				"message": map[string]string{"content": `{"tags":["creative","drawing","tool"]}`},
			}},
		})
	}))
	defer aiUpstream.Close()

	srv, db := newMetadataDeployServer(t, NewAIProxy("test-key", aiUpstream.URL, "tagger", nil, nil))
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "sketch", map[string]string{
		"index.html": `<title>Sketch Pad</title><meta name="description" content="Draw quick ideas"><h1>Draw</h1>`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	var tagsRaw string
	if err := db.QueryRow(`SELECT tags FROM sites WHERE name = ?`, "sketch").Scan(&tagsRaw); err != nil {
		t.Fatal(err)
	}
	tags := decodeSiteTags(tagsRaw)
	if len(tags) != 3 || tags[0] != "creative" || tags[1] != "drawing" || tags[2] != "tool" {
		t.Fatalf("stored tags = %v", tags)
	}
}

func TestSiteTagPromptCapsFileList(t *testing.T) {
	files := []deployFile{{path: "index.html", data: []byte("<h1>Big Site</h1>")}}
	for i := 0; i < maxTagPromptFiles+3; i++ {
		files = append(files, deployFile{path: strings.Repeat("deep/", 40) + "asset-" + string(rune('a'+i%26)) + ".js"})
	}
	prompt := siteTagPrompt("big", files, SiteMetadata{Title: "Big"})
	if !strings.Contains(prompt, "... and 4 more") {
		t.Fatalf("prompt did not summarize extra files:\n%s", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("deep/", 30)) {
		t.Fatalf("prompt includes an uncapped deep path:\n%s", prompt)
	}
}

func TestDeploySkipsAutoTagsForRestrictedSite(t *testing.T) {
	called := false
	aiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{})
	}))
	defer aiUpstream.Close()

	srv, db := newMetadataDeployServer(t, NewAIProxy("test-key", aiUpstream.URL, "tagger", nil, nil))
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>Secret</title>`,
		"_access.json": `{"allow":["alice@example.com"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("AI upstream was called for restricted site")
	}
	var tagsRaw string
	if err := db.QueryRow(`SELECT tags FROM sites WHERE name = ?`, "secret").Scan(&tagsRaw); err != nil {
		t.Fatal(err)
	}
	if tags := decodeSiteTags(tagsRaw); len(tags) != 0 {
		t.Fatalf("restricted tags = %v, want none", tags)
	}
}

func TestPublicRedeployDefersAITaggingUntilAfterAccessRemoval(t *testing.T) {
	called := false
	aiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"content": `{"tags":["public"]}`},
			}},
		})
	}))
	defer aiUpstream.Close()
	srv, db := newMetadataDeployServer(t, NewAIProxy("test-key", aiUpstream.URL, "tagger", nil, nil))
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>Old</title>`,
		"_spot.json":   `{"title":"Old title","tags":["old"]}`,
		"_access.json": `{"allow":["alice@example.com"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	called = false

	removedAccess := false
	srv.sites = observeRemoveSiteStore{
		SiteStorage: srv.sites,
		onRemove: func(path string) {
			if path == accessFileName {
				removedAccess = true
				if called {
					t.Fatal("AI tagger called before access policy was removed")
				}
			}
		},
	}
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html": `<title>New</title><h1>Public launch</h1>`,
		"_spot.json": `{"title":"New title"}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("public redeploy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !removedAccess {
		t.Fatal("access policy was not removed")
	}
	if !called {
		t.Fatal("AI tagger was not called after access removal")
	}
	var tagsRaw string
	if err := db.QueryRow(`SELECT tags FROM sites WHERE name = ?`, "secret").Scan(&tagsRaw); err != nil {
		t.Fatal(err)
	}
	if tags := decodeSiteTags(tagsRaw); len(tags) != 1 || tags[0] != "public" {
		t.Fatalf("tags after public redeploy = %v, want [public]", tags)
	}
}

func TestFailedPublicRedeployDoesNotPublishMetadata(t *testing.T) {
	srv, db := newMetadataDeployServer(t, nil)
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>Old</title>`,
		"old.txt":      `stale`,
		"_spot.json":   `{"title":"Old title","description":"Old description","tags":["old"]}`,
		"_access.json": `{"allow":["alice@example.com"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	srv.sites = failRemoveSiteStore{SiteStorage: srv.sites}
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html": `<title>New</title>`,
		"_spot.json": `{"title":"New title","description":"New description","tags":["new"]}`,
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("redeploy = %d %s, want 500", rec.Code, rec.Body.String())
	}

	var title, description, tagsRaw string
	if err := db.QueryRow(`SELECT title, description, tags FROM sites WHERE name = ?`, "secret").Scan(&title, &description, &tagsRaw); err != nil {
		t.Fatal(err)
	}
	if title != "Old title" || description != "Old description" {
		t.Fatalf("metadata = title %q description %q, want old metadata", title, description)
	}
	tags := decodeSiteTags(tagsRaw)
	if len(tags) != 1 || tags[0] != "old" {
		t.Fatalf("tags = %v, want [old]", tags)
	}
}

func TestPublicRedeployUpdatesMetadataBeforeRemovingAccessPolicy(t *testing.T) {
	srv, db := newMetadataDeployServer(t, nil)
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>Old</title>`,
		"old.txt":      `stale`,
		"_spot.json":   `{"title":"Old title","description":"Old description","tags":["old"]}`,
		"_access.json": `{"allow":["alice@example.com"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	removedAccess := false
	srv.sites = observeRemoveSiteStore{
		SiteStorage: srv.sites,
		onRemove: func(path string) {
			if path != accessFileName {
				return
			}
			removedAccess = true
			var title, description string
			if err := db.QueryRow(`SELECT title, description FROM sites WHERE name = ?`, "secret").Scan(&title, &description); err != nil {
				t.Fatal(err)
			}
			if title != "New title" || description != "New description" {
				t.Fatalf("metadata at access removal = title %q description %q, want new metadata", title, description)
			}
		},
	}
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html": `<title>New</title>`,
		"_spot.json": `{"title":"New title","description":"New description","tags":["new"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("public redeploy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !removedAccess {
		t.Fatal("access policy was not removed")
	}
}

func TestPublicPolicyReplacementUpdatesMetadataBeforeWritingAccessPolicy(t *testing.T) {
	srv, db := newMetadataDeployServer(t, nil)
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>Old</title>`,
		"_spot.json":   `{"title":"Old title","description":"Old description","tags":["old"]}`,
		"_access.json": `{"allow":["alice@example.com"]}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	wroteAccess := false
	srv.sites = observePutSiteStore{
		SiteStorage: srv.sites,
		onPut: func(path string) {
			if path != accessFileName {
				return
			}
			wroteAccess = true
			var title, description string
			if err := db.QueryRow(`SELECT title, description FROM sites WHERE name = ?`, "secret").Scan(&title, &description); err != nil {
				t.Fatal(err)
			}
			if title != "New title" || description != "New description" {
				t.Fatalf("metadata at access write = title %q description %q, want new metadata", title, description)
			}
		},
	}
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "secret", map[string]string{
		"index.html":   `<title>New</title>`,
		"_spot.json":   `{"title":"New title","description":"New description","tags":["new"]}`,
		"_access.json": `{"download":false}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("public policy redeploy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !wroteAccess {
		t.Fatal("access policy was not written")
	}
}

func TestFailedDeferredAccessRemovalRollsBackMetadata(t *testing.T) {
	srv, db := newMetadataDeployServer(t, nil)
	defer db.Close()

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "demo", map[string]string{
		"index.html":   `<title>Old</title>`,
		"_spot.json":   `{"title":"Old title","description":"Old description","tags":["old"]}`,
		"_access.json": `{"download":false}`,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", rec.Code, rec.Body.String())
	}

	srv.sites = failPathRemoveSiteStore{SiteStorage: srv.sites, path: accessFileName}
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, deployRequest(t, "spot.localhost", "demo", map[string]string{
		"index.html": `<title>New</title>`,
		"_spot.json": `{"title":"New title","description":"New description","tags":["new"]}`,
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("public redeploy = %d %s, want 500", rec.Code, rec.Body.String())
	}

	var title, description, tagsRaw string
	if err := db.QueryRow(`SELECT title, description, tags FROM sites WHERE name = ?`, "demo").Scan(&title, &description, &tagsRaw); err != nil {
		t.Fatal(err)
	}
	if title != "Old title" || description != "Old description" {
		t.Fatalf("metadata after failed deferred access removal = title %q description %q, want old metadata", title, description)
	}
	tags := decodeSiteTags(tagsRaw)
	if len(tags) != 1 || tags[0] != "old" {
		t.Fatalf("tags after failed deferred access removal = %v, want [old]", tags)
	}
}

type failRemoveSiteStore struct {
	SiteStorage
}

func (s failRemoveSiteStore) Remove(context.Context, string, string) error {
	return errors.New("remove failed")
}

func (s failRemoveSiteStore) Open(ctx context.Context, site, path string) (io.ReadCloser, SiteFileInfo, error) {
	return s.SiteStorage.Open(ctx, site, path)
}

type failPathRemoveSiteStore struct {
	SiteStorage
	path string
}

func (s failPathRemoveSiteStore) Remove(ctx context.Context, site, path string) error {
	if path == s.path {
		return errors.New("remove failed")
	}
	return s.SiteStorage.Remove(ctx, site, path)
}

type observeRemoveSiteStore struct {
	SiteStorage
	onRemove func(path string)
}

func (s observeRemoveSiteStore) Remove(ctx context.Context, site, path string) error {
	if s.onRemove != nil {
		s.onRemove(path)
	}
	return s.SiteStorage.Remove(ctx, site, path)
}

type observePutSiteStore struct {
	SiteStorage
	onPut func(path string)
}

func (s observePutSiteStore) Put(ctx context.Context, site, path, contentType string, data []byte) error {
	if s.onPut != nil {
		s.onPut(path)
	}
	return s.SiteStorage.Put(ctx, site, path, contentType, data)
}

func newMetadataDeployServer(t *testing.T, ai *AIProxy) (*Server, *sql.DB) {
	t.Helper()
	db, err := openSQLiteDB(context.Background(), t.TempDir()+"/spot.db")
	if err != nil {
		t.Fatal(err)
	}
	registry := NewSiteRegistry(db, nil)
	return &Server{
		deployAuth:     registry,
		siteAdmin:      registry,
		siteManager:    registry,
		sites:          newTestSiteStore(t),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
		dbLimit:        NewRateLimiter(1000, 1000),
		ai:             ai,
	}, db
}
