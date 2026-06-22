package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
