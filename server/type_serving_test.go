package main

import (
	"net/http"
	"strings"
	"testing"
)

const deckHost = "deck.spot.localhost"

func typedDeckEnv(t *testing.T) *typesEnv {
	t.Helper()
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustUpload(cardVersion(t, "1.1.0"))
	env.mustDeploy("deck", map[string]string{"card.json": `{"title":"Deck"}`}, map[string]string{"type": "card@1.0.0"})
	return env
}

func TestTypedSiteServesViewerAndData(t *testing.T) {
	env := typedDeckEnv(t)
	for _, target := range []string{"/", "/index.html"} {
		rec := env.get(deckHost, target)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Card 1.0.0</title>") {
			t.Fatalf("GET %s = %d %q, want the 1.0.0 entry", target, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
			t.Errorf("GET %s Cache-Control = %q", target, cc)
		}
		if rec.Header().Get("ETag") == "" || rec.Header().Get("Last-Modified") != "" {
			t.Errorf("GET %s validators: ETag %q Last-Modified %q", target, rec.Header().Get("ETag"), rec.Header().Get("Last-Modified"))
		}
	}
	js := env.get(deckHost, "/_type/card.js")
	if js.Code != http.StatusOK || !strings.Contains(js.Header().Get("Content-Type"), "javascript") || !strings.Contains(js.Body.String(), "card@1.0.0") {
		t.Fatalf("GET /_type/card.js = %d %s", js.Code, js.Header().Get("Content-Type"))
	}
	for _, target := range []string{"/_type/type.json", "/_type/SKILL.md", "/_type/", "/_type", "/_type/missing.js"} {
		if rec := env.get(deckHost, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
	data := env.get(deckHost, "/card.json")
	if data.Code != http.StatusOK || data.Body.String() != `{"title":"Deck"}` {
		t.Fatalf("GET /card.json = %d %q", data.Code, data.Body.String())
	}
	if cc := data.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("data Cache-Control = %q, want no-store", cc)
	}
	if data.Header().Get("Last-Modified") != "" || data.Header().Get("ETag") != "" {
		t.Errorf("data carries validators: %v", data.Header())
	}
	if rec := env.get(deckHost, "/missing.json"); rec.Code != http.StatusNotFound {
		t.Errorf("missing data = %d, want 404", rec.Code)
	}

	head := env.do(httptestRequest(http.MethodHead, deckHost, "/_type/card.js"))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") != js.Header().Get("ETag") {
		t.Fatalf("HEAD /_type/card.js = %d body %d etag %q", head.Code, head.Body.Len(), head.Header().Get("ETag"))
	}
}

func TestTypedSiteServesOnlyInventoryFiles(t *testing.T) {
	env := typedDeckEnv(t)
	// An object left behind by an interrupted upload is not in the inventory.
	if err := env.srv.sites.(TypeFileStorage).PutTypeFile(t.Context(), "card", "1.0.0", "orphan.js", "text/javascript", []byte("evil()")); err != nil {
		t.Fatal(err)
	}
	if rec := env.get(deckHost, "/_type/orphan.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("orphan object = %d, want 404", rec.Code)
	}
}

func TestTypedSiteConditionalRequests(t *testing.T) {
	env := typedDeckEnv(t)
	entry := env.get(deckHost, "/")
	etag := entry.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"1.0.0-`) {
		t.Fatalf("ETag = %q", etag)
	}
	for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		rec := env.get(deckHost, "/", "If-None-Match", header)
		if rec.Code != http.StatusNotModified || rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "private, no-cache" {
			t.Errorf("If-None-Match %s = %d %v, want 304", header, rec.Code, rec.Header())
		}
	}
	// If-Modified-Since alone never validates a type file.
	if rec := env.get(deckHost, "/", "If-Modified-Since", "Mon, 01 Jan 2035 00:00:00 GMT"); rec.Code != http.StatusOK {
		t.Errorf("If-Modified-Since = %d, want 200", rec.Code)
	}
	if rec := env.get(deckHost, "/card.json", "If-Modified-Since", "Mon, 01 Jan 2035 00:00:00 GMT"); rec.Code != http.StatusOK {
		t.Errorf("data If-Modified-Since = %d, want 200", rec.Code)
	}

	// A repin to another version changes the entry's tag; a repin back to
	// the older version answers with its bytes, not a stale 304.
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.1.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("repin 1.1.0 = %d %s", rec.Code, rec.Body.String())
	}
	newer := env.get(deckHost, "/", "If-None-Match", etag)
	if newer.Code != http.StatusOK || !strings.Contains(newer.Body.String(), "Card 1.1.0") {
		t.Fatalf("after repin = %d %q", newer.Code, newer.Body.String())
	}
	newerTag := newer.Header().Get("ETag")
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.0.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("repin back = %d %s", rec.Code, rec.Body.String())
	}
	older := env.get(deckHost, "/", "If-None-Match", newerTag)
	if older.Code != http.StatusOK || !strings.Contains(older.Body.String(), "Card 1.0.0") {
		t.Fatalf("repin to older = %d %q, want the 1.0.0 entry", older.Code, older.Body.String())
	}
}

func TestTypedSiteRevokedViewerNeverGets304(t *testing.T) {
	env := typedDeckEnv(t)
	etag := env.get(deckHost, "/_type/card.js").Header().Get("ETag")
	env.mustDeploy("deck", map[string]string{
		"card.json":    `{"title":"Deck"}`,
		accessFileName: `{"allow":["` + typesOwnerEmail + `"]}`,
	}, map[string]string{"type": "card@1.0.0"})
	allowed := env.get(deckHost, "/_type/card.js", "If-None-Match", etag)
	if allowed.Code != http.StatusNotModified || allowed.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("allowed viewer on restricted site = %d %q", allowed.Code, allowed.Header().Get("Cache-Control"))
	}
	if data := env.get(deckHost, "/card.json"); data.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("restricted data Cache-Control = %q", data.Header().Get("Cache-Control"))
	}
	env.as("stranger@example.com")
	for _, target := range []string{"/", "/_type/card.js", "/card.json"} {
		if rec := env.get(deckHost, target, "If-None-Match", etag); rec.Code != http.StatusForbidden {
			t.Errorf("revoked viewer GET %s = %d, want 403", target, rec.Code)
		}
	}
}

func TestUntypedSiteServingIsUnchanged(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("plain", map[string]string{"index.html": "<h1>plain</h1>", "_type/x.js": "x"}, nil)
	rec := env.get("plain.spot.localhost", "/")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "" || rec.Header().Get("Last-Modified") == "" {
		t.Fatalf("untyped index = %d %v", rec.Code, rec.Header())
	}
	// _type/ is ordinary content on an untyped site.
	if rec := env.get("plain.spot.localhost", "/_type/x.js"); rec.Code != http.StatusOK {
		t.Fatalf("untyped _type/x.js = %d", rec.Code)
	}
}

func httptestRequest(method, host, target string) *http.Request {
	req, _ := http.NewRequest(method, "http://"+host+target, nil)
	req.RemoteAddr = "192.0.2.1:1234"
	return req
}
