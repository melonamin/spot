package main

import (
	"net/http"
	"strings"
	"testing"
)

const barHost = "bar.spot.localhost"

func barLabel(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<div><b>")
	end := strings.Index(body, "</span></div>")
	if start < 0 || end < start {
		t.Fatalf("no access bar in %q", body)
	}
	return body[start+len("<div><b>") : end]
}

func TestAccessBarFollowsTheSitePolicy(t *testing.T) {
	env := newTypesEnv(t)
	cases := []struct {
		name, access, want string
	}{
		{"open", "", `OPEN</b><span title="Everyone on the mesh can open this page">Everyone on the mesh can open this page`},
		{"private", `{"allow":["` + typesOwnerEmail + `"]}`, `PRIVATE</b><span title="Only owner@example.com can open this page">Only owner@example.com can open this page`},
		{"shared", `{"allow":["` + typesOwnerEmail + `","team-ops"]}`, `SHARED</b><span title="owner@example.com, team-ops can open this page">owner@example.com, team-ops can open this page`},
		{"one group", `{"allow":["team-ops"]}`, `SHARED</b><span title="team-ops can open this page">team-ops can open this page`},
	}
	for _, tc := range cases {
		files := map[string]string{"index.html": "<!doctype html><html><head><title>x</title></head><body class=\"page\"><h1>bar</h1></body></html>"}
		if tc.access != "" {
			files[accessFileName] = tc.access
		}
		env.mustDeploy("bar", files, nil)
		if tc.name == "one group" {
			env.srv.resolver = NewStaticResolver(typesOwnerEmail, "owner", []string{"team-ops"})
		}
		rec := env.get(barHost, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET / = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if got := barLabel(t, rec.Body.String()); got != tc.want {
			t.Errorf("%s: bar = %q, want %q", tc.name, got, tc.want)
		}
		if !strings.Contains(rec.Body.String(), `<body class="page"><spot-access-bar`) {
			t.Errorf("%s: bar is not the first thing in <body>: %q", tc.name, rec.Body.String())
		}
	}
}

func TestAccessBarETagChangesWithAccess(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>"}, nil)
	open := env.get(barHost, "/")
	etag := open.Header().Get("ETag")
	if rec := env.get(barHost, "/", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("unchanged page = %d, want 304", rec.Code)
	}
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/bar/access", `{"allow":["`+typesOwnerEmail+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("restrict = %d %s", rec.Code, rec.Body.String())
	}
	rec := env.get(barHost, "/", "If-None-Match", etag)
	if rec.Code != http.StatusOK || !strings.HasPrefix(barLabel(t, rec.Body.String()), "PRIVATE") {
		t.Fatalf("after restricting = %d %q, want 200 with the PRIVATE bar", rec.Code, rec.Body.String())
	}
}

func TestAccessBarOnlyOnHTMLPages(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>", "app.js": "<body>", "data.json": `{"a":"<body>"}`}, nil)
	for _, target := range []string{"/app.js", "/data.json"} {
		if rec := env.get(barHost, target); strings.Contains(rec.Body.String(), "spot-access-bar") {
			t.Errorf("GET %s carries the bar: %q", target, rec.Body.String())
		}
	}
}

func TestAccessBarOnTypedViewer(t *testing.T) {
	env := typedDeckEnv(t)
	rec := env.get(deckHost, "/")
	if rec.Code != http.StatusOK || !strings.HasPrefix(barLabel(t, rec.Body.String()), "OPEN") {
		t.Fatalf("typed entry = %d %q", rec.Code, rec.Body.String())
	}
	if js := env.get(deckHost, "/_type/card.js"); strings.Contains(js.Body.String(), "spot-access-bar") {
		t.Fatalf("typed asset carries the bar")
	}
}

func TestAccessBarEscapesPolicyEntries(t *testing.T) {
	markup := accessBarMarkup(accessBarFor(&AccessPolicy{Allow: []string{`a"><script>x()</script>@example.com`}}))
	if strings.Contains(markup, "<script>") {
		t.Fatalf("policy entry not escaped: %q", markup)
	}
}

func TestInjectAccessBarPlacement(t *testing.T) {
	cases := map[string]string{
		"<!DOCTYPE html><HTML><BODY>x</BODY></HTML>": "<!DOCTYPE html><HTML><BODY>|x</BODY></HTML>",
		"<head><title>t</title></head><p>x":          "<head><title>t</title></head>|<p>x",
		"<!doctype html><html lang=en><p>x":          "<!doctype html><html lang=en>|<p>x",
		"<!doctype html><p>x":                        "<!doctype html>|<p>x",
		"<p>x":                                       "|<p>x",
		"<bodyguard></bodyguard><body>x":             "<bodyguard></bodyguard><body>|x",
	}
	for in, want := range cases {
		if got := string(injectAccessBar([]byte(in), "|")); got != want {
			t.Errorf("inject(%q) = %q, want %q", in, got, want)
		}
	}
}
