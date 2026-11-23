package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSDKExtensionAssets(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"20-second.js":    "window.order.push(2)",
		"10-first.js":     "window.order = [1] // trailing comment",
		"10-example.d.ts": "declare namespace SpotSDK { interface Spot { example(): void } }",
		"ignored.mjs":     "not loaded",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	assets, err := loadSDKExtensionAssets(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spot.js", "spot.d.ts"} {
		core, err := staticAssets.ReadFile("static_assets/sdk/" + name)
		if err != nil || !bytes.HasPrefix(assets[name], core) {
			t.Fatalf("%s lost core: %v", name, err)
		}
	}
	if !strings.HasSuffix(string(assets["spot.js"]), "\n;\nwindow.order = [1] // trailing comment\n\n;\nwindow.order.push(2)\n") {
		t.Fatal("SDK extensions lost ordering or script separators")
	}
	if err := os.WriteFile(filepath.Join(dir, "10-first.js"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	server := &Server{sdkAssets: assets}
	for _, name := range []string{"spot.js", "spot.d.ts"} {
		rec := httptest.NewRecorder()
		server.serveEmbeddedAsset(rec, httptest.NewRequest(http.MethodGet, "/"+name, nil), name)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), assets[name]) || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("served %s differs from startup snapshot: %d", name, rec.Code)
		}
	}
}

func TestSDKExtensionAssetsConfiguration(t *testing.T) {
	assets, err := loadSDKExtensionAssets("")
	if err != nil || assets != nil {
		t.Fatal("unset extension directory changed standalone assets")
	}
	if _, err := loadSDKExtensionAssets(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing extension directory accepted")
	}
	for _, kind := range []string{"symlink", "directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "extension.js")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("target.js", name)
			case "directory":
				err = os.Mkdir(name, 0700)
			case "oversized":
				err = os.WriteFile(name, bytes.Repeat([]byte("x"), maxSDKExtensionBytes+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadSDKExtensionAssets(dir); err == nil {
				t.Fatalf("accepted %s extension", kind)
			}
		})
	}
	t.Setenv("SPOT_SDK_EXTENSIONS_DIR", "/operator/extensions")
	cfg := defaultConfigFromEnv()
	if cfg.SDKExtensionsDir != "/operator/extensions" {
		t.Fatal("extension directory env ignored")
	}
	if err := applyCLIFlags(&cfg, []string{"--sdk-extensions-dir", "/override"}); err != nil || cfg.SDKExtensionsDir != "/override" {
		t.Fatalf("extension directory flag ignored: %v", err)
	}
}

func TestSDKExtensionAssetsRespectSiteAccess(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	st.srv.sdkAssets = map[string][]byte{"spot.js": []byte("operator extension")}
	for _, viewer := range []string{"", "stranger@example.com", "owner@example.com"} {
		req := siteRequest(http.MethodGet, "private.sites.localhost:8443", "/spot.js")
		if viewer != "" {
			req = asForwardUser(req, viewer)
		}
		rec := st.do(req)
		if viewer == "owner@example.com" {
			if rec.Code != http.StatusOK || rec.Body.String() != "operator extension" {
				t.Fatalf("owner extension asset = %d %s", rec.Code, rec.Body.String())
			}
		} else if rec.Code == http.StatusOK {
			t.Fatalf("extension asset bypassed site access for %q", viewer)
		}
	}
}
