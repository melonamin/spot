//go:build integration

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// TestTypedSiteS3Roundtrip runs the type package and typed site paths
// against real S3 storage (RustFS from `just up`, or SPOT_TEST_S3_ENDPOINT):
// package objects under _types/, inventory-only serving, conditional
// requests on the streaming path, and the preview package.
func TestTypedSiteS3Roundtrip(t *testing.T) {
	endpoint := os.Getenv("SPOT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	requireTestS3Endpoint(t, endpoint)
	store, err := NewSiteStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-sites")
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	env := newTypesEnv(t)
	env.srv.sites = store
	env.srv.policies = NewPolicyStore("", time.Minute)

	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := "it-card-" + hex.EncodeToString(suffix)
	site := name + "-site"
	files := renamedType(t, name, "1.0.0")
	ctx := context.Background()
	t.Cleanup(func() {
		for p := range files {
			store.client.RemoveObject(ctx, store.bucket, typeObjectRootPrefix+name+"/1.0.0/"+p, minio.RemoveObjectOptions{})
		}
		store.client.RemoveObject(ctx, store.bucket, typeObjectRootPrefix+name+"/1.0.0/orphan.js", minio.RemoveObjectOptions{})
		if paths, err := store.List(ctx, site); err == nil {
			for _, p := range paths {
				store.Remove(ctx, site, p)
			}
		}
	})

	env.mustUpload(files)
	env.mustDeploy(site, map[string]string{"card.json": `{"title":"S3"}`}, map[string]string{"type": name + "@1.0.0"})
	host := site + ".spot.localhost"

	entry := env.get(host, "/")
	if entry.Code != http.StatusOK || !strings.Contains(entry.Body.String(), "Card 1.0.0") ||
		entry.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("entry = %d %v %q", entry.Code, entry.Header(), entry.Body.String())
	}
	js := env.get(host, "/_type/card.js")
	if js.Code != http.StatusOK || js.Body.String() != files["card.js"] {
		t.Fatalf("viewer file = %d (%d bytes)", js.Code, js.Body.Len())
	}
	if rec := env.get(host, "/_type/card.js", "If-None-Match", js.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("conditional = %d, want 304", rec.Code)
	}
	head := env.do(httptestRequest(http.MethodHead, host, "/_type/card.css"))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d body bytes", head.Code, head.Body.Len())
	}
	data := env.get(host, "/card.json")
	if data.Code != http.StatusOK || data.Header().Get("Cache-Control") != "no-store" || data.Header().Get("Last-Modified") != "" {
		t.Fatalf("data = %d %v", data.Code, data.Header())
	}
	if err := store.PutTypeFile(ctx, name, "1.0.0", "orphan.js", "text/javascript", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if rec := env.get(host, "/_type/orphan.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("orphan object = %d, want 404", rec.Code)
	}

	skill := env.get("spot.localhost", "/api/types/"+name+"/1.0.0/skill")
	if skill.Code != http.StatusOK || skill.Body.String() != files[typeSkillFile] {
		t.Fatalf("skill = %d %q", skill.Code, skill.Body.String())
	}
	pkg := env.get("spot.localhost", "/api/types/"+name+"/1.0.0/package")
	zr, err := zip.NewReader(bytes.NewReader(pkg.Body.Bytes()), int64(pkg.Body.Len()))
	if err != nil || len(zr.File) != 4 {
		t.Fatalf("package = %d: %v", pkg.Code, err)
	}
	download := env.get(host, "/api/download")
	if download.Code != http.StatusOK || download.Header().Get("X-Spot-Type") != name+"@1.0.0" {
		t.Fatalf("download = %d %v", download.Code, download.Header())
	}
}
