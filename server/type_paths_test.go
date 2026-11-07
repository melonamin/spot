package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateContractPath(t *testing.T) {
	for _, ok := range []string{"index.html", "a/b/c.json", "slides/01-intro.html", "_spot.json", "a-b_c.d"} {
		if err := validateContractPath(ok); err != nil {
			t.Errorf("validateContractPath(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "/index.html", "a//b", "a/", "./a", "a/./b", "a/../b", "..",
		`a\b`, "a:b", "a\x00b", "a\nb", "a\x7fb"} {
		if err := validateContractPath(bad); err == nil {
			t.Errorf("validateContractPath(%q) accepted an invalid path", bad)
		}
	}
}

func TestDataPathCoversIsSegmentBounded(t *testing.T) {
	entries := []string{"card.json", "media/"}
	for p, want := range map[string]bool{
		"card.json":       true,
		"card.json.bak":   false,
		"media/x.png":     true,
		"media/a/b.png":   true,
		"mediax.png":      false,
		"media":           false,
		"other/card.json": false,
	} {
		if got := dataPathCovers(entries, p); got != want {
			t.Errorf("dataPathCovers(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestReservedDataSegment(t *testing.T) {
	for _, entry := range []string{"index.html", "_type/", "_type/x.js", "_access.json", "_spot.json",
		"_screenshot.png", "_screenshot.anything", "spot.js", "spot-live.js", "spot.d.ts", "api/", "api/x.json",
		"INDEX.HTML", "_Type/"} {
		if !reservedDataSegment(entry) {
			t.Errorf("reservedDataSegment(%q) = false, want reserved", entry)
		}
	}
	for _, entry := range []string{"project/index.html", "project/", "data.json", "apis/", "spot.json", "types/"} {
		if reservedDataSegment(entry) {
			t.Errorf("reservedDataSegment(%q) = true, want allowed", entry)
		}
	}
}

func TestRejectInvalidSitePathsBeforeTheMux(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t)}
	handler := srv.rejectInvalidSitePaths(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	for _, tt := range []struct {
		host, target string
		want         int
	}{
		{"demo.spot.localhost", "/", http.StatusTeapot},
		{"demo.spot.localhost", "/project/deck.json", http.StatusTeapot},
		{"demo.spot.localhost", "/project/", http.StatusTeapot},
		{"demo.spot.localhost", "/_type/app.js?x=1", http.StatusTeapot},
		{"demo.spot.localhost", "/a%20b.html", http.StatusTeapot},
		{"demo.spot.localhost", "/_type%2Fapp.js", http.StatusBadRequest},
		{"demo.spot.localhost", "/_type%2fapp.js", http.StatusBadRequest},
		{"demo.spot.localhost", "/a%5Cb", http.StatusBadRequest},
		{"demo.spot.localhost", "/project/../_type/app.js", http.StatusBadRequest},
		{"demo.spot.localhost", "/project/%2e%2e/_type/app.js", http.StatusBadRequest},
		{"demo.spot.localhost", "/./index.html", http.StatusBadRequest},
		{"demo.spot.localhost", "//index.html", http.StatusBadRequest},
		{"demo.spot.localhost", "/a//b", http.StatusBadRequest},
		{"demo.spot.localhost", "/a%00b", http.StatusBadRequest},
		// The apex keeps its own routing; the contract covers site origins.
		{"spot.localhost", "/a//b", http.StatusTeapot},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tt.host+tt.target, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s%s = %d, want %d", tt.host, tt.target, rec.Code, tt.want)
		}
	}
}
