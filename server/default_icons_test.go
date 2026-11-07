package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDefaultIconsServer(t *testing.T, iconsDir string) (http.Handler, SiteStorage) {
	t.Helper()
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		resolver:        NewStaticResolver("owner@spot.local", "Owner", nil),
		policies:        NewPolicyStore(filepath.Join(root, "sites"), time.Second),
		sites:           sites,
		spotDomain:      "spot.localhost",
		trustedProxies:  defaultTrustedProxies,
		serveStatic:     true,
		defaultIconsDir: iconsDir,
	}
	return srv.routes(), sites
}

func getDefaultIconsPath(handler http.Handler, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestDefaultIconsAreServedWhenASiteHasNone(t *testing.T) {
	handler, sites := newDefaultIconsServer(t, "")
	if err := sites.Put(context.Background(), "demo", "index.html", "text/html", []byte("<h1>demo</h1>")); err != nil {
		t.Fatal(err)
	}
	embeddedICO, err := embeddedDefaultIcons.ReadFile("default_icons/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	embeddedTouch, err := embeddedDefaultIcons.ReadFile("default_icons/apple-touch-icon.png")
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, url, contentType string
		want                   []byte
	}{
		{"site favicon", "https://demo.spot.localhost/favicon.ico", "image/x-icon", embeddedICO},
		{"site touch icon", "https://demo.spot.localhost/apple-touch-icon.png", "image/png", embeddedTouch},
		{"site precomposed touch icon", "https://demo.spot.localhost/apple-touch-icon-precomposed.png", "image/png", embeddedTouch},
		{"unknown site, so its 404 page has an icon", "https://missing.spot.localhost/favicon.ico", "image/x-icon", embeddedICO},
		{"apex favicon", "https://spot.localhost/favicon.ico", "image/x-icon", embeddedICO},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := getDefaultIconsPath(handler, tt.url)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), tt.want) {
				t.Fatalf("code = %d, %d bytes, want %d bytes", rec.Code, rec.Body.Len(), len(tt.want))
			}
			if got := rec.Header().Get("Content-Type"); got != tt.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tt.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("Cache-Control = %q, want no-cache", got)
			}
		})
	}

	if rec := getDefaultIconsPath(handler, "https://demo.spot.localhost/nested/favicon.ico"); rec.Code != http.StatusNotFound {
		t.Fatalf("nested favicon = %d, want 404", rec.Code)
	}
	if rec := getDefaultIconsPath(handler, "https://demo.spot.localhost/other.png"); rec.Code != http.StatusNotFound {
		t.Fatalf("other missing file = %d, want 404", rec.Code)
	}
}

func TestSiteOwnIconWinsOverDefault(t *testing.T) {
	handler, sites := newDefaultIconsServer(t, "")
	if err := sites.Put(context.Background(), "demo", "favicon.ico", "image/x-icon", []byte("site icon")); err != nil {
		t.Fatal(err)
	}
	rec := getDefaultIconsPath(handler, "https://demo.spot.localhost/favicon.ico")
	if rec.Code != http.StatusOK || rec.Body.String() != "site icon" {
		t.Fatalf("code = %d body %q, want the site's own file", rec.Code, rec.Body.String())
	}
}

func TestDefaultIconsDirReplacesBuiltInIcons(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "favicon.ico"), []byte("operator icon"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler, _ := newDefaultIconsServer(t, dir)

	for _, url := range []string{"https://missing.spot.localhost/favicon.ico", "https://spot.localhost/favicon.ico"} {
		rec := getDefaultIconsPath(handler, url)
		if rec.Code != http.StatusOK || rec.Body.String() != "operator icon" {
			t.Fatalf("%s = %d body %q, want the operator's icon", url, rec.Code, rec.Body.String())
		}
	}

	// A file the operator left out must not fall back to the built-in brand.
	if rec := getDefaultIconsPath(handler, "https://spot.localhost/apple-touch-icon.png"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing operator touch icon = %d, want 404", rec.Code)
	}
}

func TestDefaultIconFallbackIgnoresOtherRepresentationModifiedTime(t *testing.T) {
	dir := t.TempDir()
	iconPath := filepath.Join(dir, "favicon.ico")
	operatorTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	writeOperatorIcon := func(content string) {
		t.Helper()
		if err := os.WriteFile(iconPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(iconPath, operatorTime, operatorTime); err != nil {
			t.Fatal(err)
		}
	}
	writeOperatorIcon("operator icon")
	handler, sites := newDefaultIconsServer(t, dir)
	if err := sites.Put(t.Context(), "demo", "favicon.ico", "image/x-icon", []byte("site icon")); err != nil {
		t.Fatal(err)
	}
	own := getDefaultIconsPath(handler, "https://demo.spot.localhost/favicon.ico")
	modified := own.Header().Get("Last-Modified")
	if own.Code != http.StatusOK || modified == "" {
		t.Fatalf("site icon = %d with Last-Modified %q", own.Code, modified)
	}
	if err := sites.Remove(t.Context(), "demo", "favicon.ico"); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"operator icon", "replacement operator icon"} {
		// Operator files may be older or retain their mtime when replaced.
		writeOperatorIcon(content)
		req := httptest.NewRequest(http.MethodGet, "https://demo.spot.localhost/favicon.ico", nil)
		// A previously cached icon may have only a time validator.
		req.Header.Set("If-Modified-Since", modified)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != content {
			t.Fatalf("fallback = %d %q, want 200 %q", rec.Code, rec.Body.String(), content)
		}
		if got := rec.Header().Get("Last-Modified"); got != "" {
			t.Fatalf("fallback Last-Modified = %q, want no time validator", got)
		}
	}
}

func TestRestrictedDefaultIconsRequireAccess(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("private", map[string]string{
		"index.html":   "private",
		accessFileName: `{"allow":["` + typesOwnerEmail + `"]}`,
	}, nil)
	for iconPath := range defaultIconFiles {
		rec := env.get("private.spot.localhost", iconPath)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("allowed %s = %d %v", iconPath, rec.Code, rec.Header())
		}
	}
	env.as("stranger@example.com")
	for iconPath := range defaultIconFiles {
		rec := env.get("private.spot.localhost", iconPath, "If-Modified-Since", "Mon, 01 Jan 2035 00:00:00 GMT")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("denied %s = %d, want 403", iconPath, rec.Code)
		}
	}
}
