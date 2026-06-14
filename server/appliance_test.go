package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteDocStorePublishesInProcess(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	hub := NewHub()
	store := &DocStore{db: db, hub: hub}
	events := make(chan Event, 4)
	hub.Subscribe("demo", "notes", events)

	created, err := store.Create(ctx, "demo", "notes", map[string]any{"title": "hello"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ev := nextEvent(t, events)
	if ev.Type != "create" || ev.ID != created.ID || ev.Doc == nil || ev.Doc.Data["title"] != "hello" {
		t.Fatalf("create event = %+v, created=%+v", ev, created)
	}

	if _, err := store.Update(ctx, "demo", "notes", created.ID, map[string]any{"title": "bye"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	ev = nextEvent(t, events)
	if ev.Type != "update" || ev.ID != created.ID || ev.Doc == nil || ev.Doc.Data["title"] != "bye" {
		t.Fatalf("update event = %+v", ev)
	}

	if err := store.Delete(ctx, "demo", "notes", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	ev = nextEvent(t, events)
	if ev.Type != "delete" || ev.ID != created.ID || ev.Doc != nil {
		t.Fatalf("delete event = %+v", ev)
	}
}

func TestSQLiteSiteRegistry(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	registry := &SiteRegistry{db: db}
	actor := Identity{Email: "owner@example.com", Name: "Owner"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", actor)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if authz.Action != "create" {
		t.Fatalf("first action = %q, want create", authz.Action)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: actor, Action: authz.Action, Status: "success", FileCount: 2, TotalBytes: 12,
	}); err != nil {
		t.Fatalf("record deploy: %v", err)
	}
	owned, err := registry.SitesOwnedBy(ctx, actor)
	if err != nil {
		t.Fatalf("owned: %v", err)
	}
	if len(owned) != 1 || owned[0].Name != "demo" || owned[0].FileCount != 2 || owned[0].TotalBytes != 12 {
		t.Fatalf("owned = %+v", owned)
	}
}

func TestLocalStores(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(ctx, "demo", "index.html", "text/html", []byte("<h1>hi</h1>")); err != nil {
		t.Fatalf("site put: %v", err)
	}
	paths, err := sites.List(ctx, "demo")
	if err != nil || len(paths) != 1 || paths[0] != "index.html" {
		t.Fatalf("site list = %v, %v", paths, err)
	}
	rc, info, err := sites.Open(ctx, "demo", "index.html")
	if err != nil {
		t.Fatalf("site open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "<h1>hi</h1>" || info.LastModified.IsZero() {
		t.Fatalf("site open body=%q info=%+v", body, info)
	}

	files, err := NewLocalFileStore(filepath.Join(root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := files.Put(ctx, "demo", "hello.txt", "text/plain", bytes.NewBufferString("hello"), 5)
	if err != nil {
		t.Fatalf("file put: %v", err)
	}
	rc, contentType, err := files.Get(ctx, "demo", stored.ID, stored.Name)
	if err != nil {
		t.Fatalf("file get: %v", err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "hello" || contentType != "text/plain; charset=utf-8" {
		t.Fatalf("file body=%q contentType=%q", body, contentType)
	}
	if err := files.RemoveSite(ctx, "demo"); err != nil {
		t.Fatalf("remove uploads: %v", err)
	}
	if _, _, err := files.Get(ctx, "demo", stored.ID, stored.Name); err != ErrNotFound {
		t.Fatalf("get removed = %v, want ErrNotFound", err)
	}
}

func TestStaticServerServesApexAndSiteFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(ctx, "demo", "index.html", "text/html", []byte("<h1>demo</h1>")); err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(ctx, "demo", "docs/index.html", "text/html", []byte("<h1>docs</h1>")); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		resolver:       NewStaticResolver("owner@spot.local", "Owner", nil),
		policies:       NewPolicyStore(filepath.Join(root, "sites"), time.Second),
		sites:          sites,
		spotDomain:     "spot.localhost",
		trustedProxies: defaultTrustedProxies,
		serveStatic:    true,
	}
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "https://spot.localhost/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("Launch")) {
		t.Fatalf("apex = %d body %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}

	for _, tt := range []struct {
		path string
		want string
	}{
		{"/install.sh", "Install the Spot CLI"},
		{"/agent.md", "Spot Agent Setup"},
		{"/spot", "usage: spot <command>"},
	} {
		req = httptest.NewRequest(http.MethodGet, "https://spot.localhost"+tt.path, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(tt.want)) {
			t.Fatalf("%s = %d body %q", tt.path, rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
		}
	}

	req = httptest.NewRequest(http.MethodGet, "https://demo.spot.localhost/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>demo</h1>" {
		t.Fatalf("site = %d body %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "https://demo.spot.localhost/docs", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/" {
		t.Fatalf("site directory redirect = %d location %q", rec.Code, rec.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "https://demo.spot.localhost/docs/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>docs</h1>" {
		t.Fatalf("site directory index = %d body %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "https://demo.spot.localhost/spot.js", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("window.spot")) {
		t.Fatalf("spot.js = %d body %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
}

func nextEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}
