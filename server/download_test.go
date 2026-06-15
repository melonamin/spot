package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestCleanDownloadPaths(t *testing.T) {
	got := cleanDownloadPaths([]string{
		"index.html",
		"css/app.css",
		"",
		".",
		"..",
		"/etc/passwd",
		"../secret",
		"img/../secret",
		`windows\path.txt`,
		"C:/tmp/x",
		"C:tmp/x",
		"a:b",
		"with\x00nul",
		"about.html",
	})
	want := []string{"about.html", "css/app.css", "index.html"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanDownloadPaths = %#v, want %#v", got, want)
	}
}

// downloadServer builds a Server backed by a single temp dir: the
// LocalSiteStore serves a site's files from <dir>/<site> and the
// PolicyStore reads <dir>/<site>/_access.json, so writeSiteFile seeds
// both at once. Identity comes from the stub NetBird API where peer
// 100.64.0.7 is sasha@example.com and 100.64.0.9 is unidentified.
func downloadServer(t *testing.T, dir string) *Server {
	t.Helper()
	requests := 0
	api := newNetbirdAPI(t, &requests)
	t.Cleanup(api.Close)
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	return &Server{
		sites:          sites,
		policies:       NewPolicyStore(dir, 0),
		resolver:       NewNetbirdResolver(api.URL, "test-token", time.Minute),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		fileLimit:      NewRateLimiter(1000, 1000),
	}
}

func downloadRequest(host, peerIP string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://spot-api/api/download", nil)
	req.Header.Set("X-Forwarded-Host", host)
	if peerIP != "" {
		req.Header.Set("X-Forwarded-For", peerIP)
	}
	return req
}

func TestHandleSiteDownloadDeniesRestrictedCaller(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "secret", accessFileName, `{"allow": ["sasha@example.com"]}`)
	writeSiteFile(t, dir, "secret", "index.html", "<h1>secret</h1>")
	srv := downloadServer(t, dir)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, downloadRequest("secret.spot.localhost", "100.64.0.9"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted download by denied peer = %d, want 403", rec.Code)
	}
	// The denial must land before any zip is streamed.
	if ct := rec.Header().Get("Content-Type"); ct == "application/zip" {
		t.Fatalf("denied download streamed a zip (Content-Type %q)", ct)
	}
}

func TestHandleSiteDownloadRespectsDownloadOptOut(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "nodownload", accessFileName, `{"download": false}`)
	writeSiteFile(t, dir, "nodownload", "index.html", "<h1>hi</h1>")
	srv := downloadServer(t, dir)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, downloadRequest("nodownload.spot.localhost", "100.64.0.7"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("download from opt-out site = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "application/zip" {
		t.Fatalf("opt-out download streamed a zip (Content-Type %q)", ct)
	}
}

func TestHandleSiteDownloadFailsClosedOnBrokenPolicy(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "broken", accessFileName, `{not json`)
	writeSiteFile(t, dir, "broken", "index.html", "<h1>hi</h1>")
	srv := downloadServer(t, dir)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, downloadRequest("broken.spot.localhost", "100.64.0.7"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("download with broken policy = %d, want 503 (fail closed)", rec.Code)
	}
}

func TestHandleSiteDownloadOpenSiteReturnsZip(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "open", "index.html", "<h1>hi</h1>")
	writeSiteFile(t, dir, "open", "css/app.css", "body{}")
	srv := downloadServer(t, dir)

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, downloadRequest("open.spot.localhost", "10.0.0.1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("open site download = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("response is not a valid zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s in zip: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s in zip: %v", f.Name, err)
		}
		got[f.Name] = string(data)
	}
	want := map[string]string{
		"index.html":  "<h1>hi</h1>",
		"css/app.css": "body{}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zip contents = %#v, want %#v", got, want)
	}
}
