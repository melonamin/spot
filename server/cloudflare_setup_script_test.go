package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestSetupCloudflarePagesTokenScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	var sawCreate bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bootstrap" {
			t.Fatalf("Authorization = %q, want bootstrap bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /user/tokens/permission_groups":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{
				{"id": "pages", "name": "Cloudflare Pages Write"},
				{"id": "dns", "name": "DNS Write"},
				{"id": "zone", "name": "Zone Read"},
			}})
		case "POST /user/tokens":
			sawCreate = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["name"] != "spot-cloudflare-pages-runtime" {
				t.Fatalf("token name = %v", body["name"])
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"value": "runtime-token"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	cmd := exec.Command("sh", "../scripts/setup-cloudflare-pages-token.sh",
		"--bootstrap-token", "bootstrap",
		"--account-id", "acct",
		"--zone-id", "zone-id",
		"--base-domain", "pages.example.com")
	cmd.Env = append(cmd.Environ(), "CLOUDFLARE_API_BASE_URL="+ts.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup script failed: %v\n%s", err, out)
	}
	if !sawCreate {
		t.Fatal("script did not create a token")
	}
	got := string(out)
	for _, want := range []string{
		"SPOT_CLOUDFLARE_API_TOKEN=runtime-token",
		"SPOT_CLOUDFLARE_ACCOUNT_ID=acct",
		"SPOT_CLOUDFLARE_ZONE_ID=zone-id",
		"SPOT_CLOUDFLARE_BASE_DOMAIN=pages.example.com",
		"SPOT_CLOUDFLARE_PROJECT_PREFIX=spot-",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("script output = %q, want %q", got, want)
		}
	}
}
