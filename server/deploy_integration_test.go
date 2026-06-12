//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestDeploySyncRoundtrip exercises the deploy handler against the real
// RustFS from the compose stack (`just up` first): a wrapped folder
// deploys with its root stripped, and a redeploy removes stale files.
func TestDeploySyncRoundtrip(t *testing.T) {
	endpoint := os.Getenv("SPOT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	sites, err := NewSiteStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-sites")
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	srv := &Server{sites: sites, spotDomain: "spot.localhost"}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	const site = "it-deploy"
	ctx := context.Background()
	cleanup := func() {
		paths, err := sites.List(ctx, site)
		if err != nil {
			t.Logf("cleanup list: %v", err)
			return
		}
		for _, p := range paths {
			if err := sites.Remove(ctx, site, p); err != nil {
				t.Logf("cleanup remove %s: %v", p, err)
			}
		}
	}
	cleanup()
	defer cleanup()

	deploy := func(files map[string]string) (int, map[string]any) {
		t.Helper()
		req := deployRequest(t, "spot.localhost", site, files)
		res, err := http.DefaultClient.Do(mustOutbound(t, req, ts.URL+"/api/deploy"))
		if err != nil {
			t.Fatalf("deploy: %v", err)
		}
		defer res.Body.Close()
		var body map[string]any
		raw, _ := io.ReadAll(res.Body)
		json.Unmarshal(raw, &body)
		if res.StatusCode != http.StatusOK {
			t.Logf("deploy response (is `just up` running?): %s", raw)
		}
		return res.StatusCode, body
	}

	code, body := deploy(map[string]string{
		"web/index.html":   "<h1>v1</h1>",
		"web/css/app.css":  "body{}",
		"web/old.txt":      "stale",
		"web/.DS_Store":    "junk",
		"web/_access.json": `{"allow":["it@example.com"]}`,
	})
	if code != http.StatusOK {
		t.Fatalf("first deploy: status %d", code)
	}
	if body["files"].(float64) != 4 || body["site"] != site {
		t.Errorf("first deploy response = %v", body)
	}

	paths, err := sites.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(paths)
	want := []string{"_access.json", "css/app.css", "index.html", "old.txt"}
	if len(paths) != len(want) {
		t.Fatalf("stored paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("stored paths = %v, want %v", paths, want)
		}
	}

	// The wrapped root must be stripped and the content type set by
	// extension, so Caddy and viewers see real HTML.
	obj, err := sites.client.GetObject(ctx, sites.bucket, site+"/index.html", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get index.html: %v", err)
	}
	defer obj.Close()
	content, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if string(content) != "<h1>v1</h1>" {
		t.Errorf("index.html = %q, want v1", content)
	}
	stat, err := obj.Stat()
	if err != nil {
		t.Fatalf("stat index.html: %v", err)
	}
	if stat.ContentType != "text/html; charset=utf-8" {
		t.Errorf("index.html content type = %q, want text/html", stat.ContentType)
	}

	// Redeploy without old.txt: sync semantics must remove it.
	code, body = deploy(map[string]string{
		"index.html":  "<h1>v2</h1>",
		"css/app.css": "body{color:red}",
	})
	if code != http.StatusOK {
		t.Fatalf("second deploy: status %d", code)
	}
	paths, err = sites.List(ctx, site)
	if err != nil {
		t.Fatalf("list after redeploy: %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != "css/app.css" || paths[1] != "index.html" {
		t.Errorf("paths after redeploy = %v, want [css/app.css index.html]", paths)
	}
}

// mustOutbound rewrites an httptest request (built for in-process
// ServeHTTP) into a real outbound request against the test server.
func mustOutbound(t *testing.T, req *http.Request, url string) *http.Request {
	t.Helper()
	out, err := http.NewRequest(req.Method, url, req.Body)
	if err != nil {
		t.Fatal(err)
	}
	out.Header = req.Header
	return out
}
