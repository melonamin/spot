package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const barHost = "bar.spot.localhost"

func barText(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<div><b>")
	end := strings.Index(body, "</span></div>")
	if start < 0 || end < start {
		t.Fatalf("no access bar in %q", body)
	}
	label := body[start+len("<div><b>") : end]
	label = label[:strings.Index(label, "</b>")] + " " + label[strings.LastIndex(label, ">")+1:]
	return label
}

func TestAccessBarDescribesPolicyWithoutNames(t *testing.T) {
	env := newTypesEnv(t)
	env.srv.resolver = NewStaticResolver(typesOwnerEmail, "owner", []string{"team-ops"})
	page := "<!doctype html><html><head><title>x</title></head><body class=\"page\"><h1>bar</h1></body></html>"
	cases := []struct {
		name, access, want string
	}{
		{"open", "", "OPEN Anyone who can reach this Spot can open this page"},
		{"private", `{"allow":["` + typesOwnerEmail + `"]}`, "PRIVATE Only you can open this page"},
		{"private, duplicate spellings", `{"allow":["` + typesOwnerEmail + `"," OWNER@example.com ",""]}`, "PRIVATE Only you can open this page"},
		{"shared", `{"allow":["` + typesOwnerEmail + `","bob@example.com","team-ops","Team-Ops"]}`, "SHARED Limited to 2 email addresses and 1 group"},
		{"one group", `{"allow":["team-ops"]}`, "SHARED Limited to 1 group"},
	}
	for _, tc := range cases {
		files := map[string]string{"index.html": page}
		if tc.access != "" {
			files[accessFileName] = tc.access
		}
		env.mustDeploy("bar", files, nil)
		rec := env.get(barHost, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET / = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if got := barText(t, body); got != tc.want {
			t.Errorf("%s: bar = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(body, "bob@example.com") || strings.Contains(body, "team-ops") || strings.Contains(body, typesOwnerEmail) {
			t.Errorf("%s: bar names allowed visitors: %q", tc.name, body)
		}
		if !strings.Contains(body, `<body class="page"><spot-access-bar`) {
			t.Errorf("%s: bar is not first in <body>: %q", tc.name, body)
		}
	}
}

func TestAccessBarOpenAudienceFollowsDeployment(t *testing.T) {
	cases := []struct {
		method       string
		login, proxy bool
		want         string
	}{
		{"netbird", false, false, "Everyone on the mesh"},
		{"tailscale", false, false, "Everyone on the tailnet"},
		{"netbird", true, false, "Anyone who can reach this Spot"},
		{"netbird", false, true, "Anyone who can reach this Spot"},
		{"tailscale", false, true, "Anyone who can reach this Spot"},
		{"single_user", false, false, "Anyone who can reach this Spot"},
		{"dev", false, false, "Anyone who can reach this Spot"},
		{"", false, true, "Anyone who can reach this Spot"},
	}
	for _, tc := range cases {
		s := &Server{deployAuthMethod: tc.method}
		if tc.login {
			s.login = &DelegatedLogin{}
		}
		if tc.proxy {
			s.forwardAuth = &ForwardAuth{}
		}
		if got := s.openAudience(); got != tc.want {
			t.Errorf("method %q login %v proxy %v = %q, want %q", tc.method, tc.login, tc.proxy, got, tc.want)
		}
	}
}

// In single-user and dev mode every visitor is the same identity, so a
// restriction that admits it admits anyone who can reach Spot.
func TestAccessBarFixedIdentityIsOpen(t *testing.T) {
	env := newTypesEnv(t)
	env.srv.deployAuthMethod = "single_user"
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>", accessFileName: `{"allow":["` + typesOwnerEmail + `"]}`}, nil)
	rec := env.get(barHost, "/")
	if got := barText(t, rec.Body.String()); got != "OPEN Anyone who can reach this Spot can open this page" {
		t.Fatalf("bar = %q", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("restricted site Cache-Control = %q, want no-store", cc)
	}
}

func TestAccessBarScopesToSpotCopyWhenPublishedToCloudflare(t *testing.T) {
	env := newTypesEnv(t)
	env.srv.cloudflarePubs = NewCloudflarePublicationStore(env.db)
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>", accessFileName: `{"allow":["` + typesOwnerEmail + `"]}`}, nil)
	plain := env.get(barHost, "/")
	if got := barText(t, plain.Body.String()); got != "PRIVATE Only you can open this page" {
		t.Fatalf("before publication: %q", got)
	}
	if err := env.srv.cloudflarePubs.InsertReservation(t.Context(), cloudflarePublication{Site: "bar", AccessMode: cloudflareAccessPublic, Status: "reserving"}); err != nil {
		t.Fatal(err)
	}
	rec := env.get(barHost, "/", "If-None-Match", plain.Header().Get("ETag"))
	if rec.Code != http.StatusOK || barText(t, rec.Body.String()) != "PRIVATE Only you can open this Spot copy" {
		t.Fatalf("after publication = %d %q", rec.Code, rec.Body.String())
	}
}

// flippingPolicyStore answers the first _access.json read with no policy and
// every later one with a restriction, as if access changed mid-request.
type flippingPolicyStore struct {
	SiteStorage
	reads int
}

func (s *flippingPolicyStore) Open(ctx context.Context, site, p string) (io.ReadCloser, SiteFileInfo, error) {
	if p == accessFileName {
		s.reads++
		if s.reads == 1 {
			return nil, SiteFileInfo{}, ErrNotFound
		}
		return io.NopCloser(strings.NewReader(`{"allow":["` + typesOwnerEmail + `"]}`)), SiteFileInfo{}, nil
	}
	return s.SiteStorage.Open(ctx, site, p)
}

func TestAccessBarUsesThePolicyThatAuthorizedTheRequest(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>"}, nil)
	env.srv.policies = nil
	env.srv.sites = &flippingPolicyStore{SiteStorage: env.srv.sites}
	rec := env.get(barHost, "/")
	if rec.Code != http.StatusOK || !strings.HasPrefix(barText(t, rec.Body.String()), "OPEN") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("GET / = %d %v %q, want the OPEN bar of the policy that admitted it", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestAccessBarValidatorsAndRanges(t *testing.T) {
	env := newTypesEnv(t)
	page := "<!doctype html><html><head><title>t</title></head><body><p>" + strings.Repeat("x", 4000) + "</p></body></html>"
	env.mustDeploy("bar", map[string]string{"index.html": page}, nil)
	full := env.get(barHost, "/")
	etag := full.Header().Get("ETag")
	if full.Code != http.StatusOK || len(etag) != 66 || full.Header().Get("Last-Modified") != "" {
		t.Fatalf("GET / = %d etag %q headers %v", full.Code, etag, full.Header())
	}
	if rec := env.get(barHost, "/", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", rec.Code)
	}
	head := env.do(httptestRequest(http.MethodHead, barHost, "/"))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != full.Header().Get("Content-Length") {
		t.Fatalf("HEAD = %d body %d length %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
	// A range that starts in the page, crosses the whole bar and ends in the page.
	body := full.Body.Bytes()
	start := bytes.Index(body, []byte("<spot-access-bar")) - 5
	end := bytes.Index(body, []byte("<p>")) + 5
	rec := env.get(barHost, "/", "Range", "bytes="+strconv.Itoa(start)+"-"+strconv.Itoa(end))
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body[start:end+1]) {
		t.Fatalf("range = %d %q, want %q", rec.Code, rec.Body.String(), body[start:end+1])
	}
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/bar/access", `{"allow":["`+typesOwnerEmail+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("restrict = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.get(barHost, "/", "If-None-Match", etag); rec.Code != http.StatusOK || !strings.HasPrefix(barText(t, rec.Body.String()), "PRIVATE") {
		t.Fatalf("after restricting = %d, want 200 with the PRIVATE bar", rec.Code)
	}
}

func TestAccessBarOnlyOnHTMLResponses(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("bar", map[string]string{
		"index.html": "<h1>bar</h1>",
		"app.js":     "document.body.innerHTML = '<body>'",
		"data.json":  `{"a":"<body>"}`,
		"guide":      "<!doctype html><html><body><p>extensionless</p></body></html>",
	}, nil)
	for _, target := range []string{"/app.js", "/data.json"} {
		if rec := env.get(barHost, target); strings.Contains(rec.Body.String(), "spot-access-bar") {
			t.Errorf("GET %s carries the bar: %q", target, rec.Body.String())
		}
	}
	guide := env.get(barHost, "/guide")
	if !strings.HasPrefix(guide.Header().Get("Content-Type"), "text/html") || !strings.Contains(guide.Body.String(), "<body><spot-access-bar") {
		t.Fatalf("extensionless HTML = %q %q", guide.Header().Get("Content-Type"), guide.Body.String())
	}
}

func TestAccessBarOnTypedSites(t *testing.T) {
	env := typedDeckEnv(t)
	entry := env.get(deckHost, "/")
	if entry.Code != http.StatusOK || !strings.HasPrefix(barText(t, entry.Body.String()), "OPEN") {
		t.Fatalf("typed entry = %d %q", entry.Code, entry.Body.String())
	}
	if js := env.get(deckHost, "/_type/card.js"); strings.Contains(js.Body.String(), "spot-access-bar") {
		t.Fatalf("typed asset carries the bar")
	}
	// HTML under a declared data path gets the bar but, like all typed data, no validator.
	env.mustDeploy("deck", map[string]string{"card.json": `{"title":"Deck"}`, "media/intro.html": "<p>intro</p>"}, map[string]string{"type": "card@1.0.0"})
	data := env.get(deckHost, "/media/intro.html")
	if data.Code != http.StatusOK || !strings.Contains(data.Body.String(), "spot-access-bar") {
		t.Fatalf("typed HTML data = %d %q", data.Code, data.Body.String())
	}
	if data.Header().Get("ETag") != "" || data.Header().Get("Last-Modified") != "" || data.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("typed HTML data headers = %v", data.Header())
	}
}

func TestAccessBarInsertOffset(t *testing.T) {
	longScript := "<script>" + strings.Repeat("var a = '<body>';\n", 200000) + "</script>"
	cases := map[string]string{
		"<!DOCTYPE html><HTML><BODY>x</BODY></HTML>":                     "<!DOCTYPE html><HTML><BODY>|x</BODY></HTML>",
		"<head><title>t</title></head><p>x":                              "<head><title>t</title></head>|<p>x",
		"<!doctype html><html lang=en><p>x":                              "<!doctype html><html lang=en>|<p>x",
		"<!doctype html><p>x":                                            "<!doctype html>|<p>x",
		"<p>x":                                                           "|<p>x",
		"plain text":                                                     "|plain text",
		"<bodyguard></bodyguard><body>x":                                 "|<bodyguard></bodyguard><body>x",
		"<head><script>let b = '<body>'</script></head><body>x":          "<head><script>let b = '<body>'</script></head><body>|x",
		"<head><!-- <body> --></head><body>x":                            "<head><!-- <body> --></head><body>|x",
		`<body data-x="a>b" class=c>x`:                                   `<body data-x="a>b" class=c>|x`,
		"<head><title>İstanbul ÅÆ</title></head><body>x":                 "<head><title>İstanbul ÅÆ</title></head><body>|x",
		"<head><template><p>t</p></template><style>p{}</style></head>x":  "<head><template><p>t</p></template><style>p{}</style></head>|x",
		"\xEF\xBB\xBF<p>x":                                               "\xEF\xBB\xBF|<p>x",
		"<html><head>" + longScript + "</head><body>x":                   "<html><head>" + longScript + "</head><body>|x",
		"<!doctype html><html><head><meta charset=utf-8></head><body>\n": "<!doctype html><html><head><meta charset=utf-8></head><body>|\n",
		"": "|",
		"<head>&#32;<title>T</title></head><body>x": "<head>&#32;<title>T</title></head><body>|x",
		"<head>&nbsp;<title>T</title></head>":       "<head>|&nbsp;<title>T</title></head>",
	}
	for in, want := range cases {
		at, err := accessBarInsertOffset(strings.NewReader(in))
		if err != nil {
			t.Fatalf("offset(%.60q): %v", in, err)
		}
		if got := in[:at] + "|" + in[at:]; got != want {
			t.Errorf("insert(%.80q) = %.80q, want %.80q", in, got[max(0, int(at)-40):], want[max(0, int(at)-40):])
		}
	}
}

// A store that cannot seek is spooled, and the response is the same.
func TestAccessBarNonSeekableSource(t *testing.T) {
	page := "<html><body><p>" + strings.Repeat("y", 70000) + "</p></body></html>"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+barHost+"/", nil)
	writeHTMLWithAccessBar(rec, req, "index.html", "text/html; charset=utf-8", "|", io.MultiReader(strings.NewReader(page)))
	if want := strings.Replace(page, "<body>", "<body>|", 1); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("non-seekable = %d, %d bytes, want %d", rec.Code, rec.Body.Len(), len(want))
	}
}

// nonSeekableStore hands out site files as plain readers, as a store without
// seeking would.
type nonSeekableStore struct {
	SiteStorage
}

func (s nonSeekableStore) Open(ctx context.Context, site, p string) (io.ReadCloser, SiteFileInfo, error) {
	rc, info, err := s.SiteStorage.Open(ctx, site, p)
	if err != nil {
		return nil, info, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(rc), rc}, info, nil
}

func TestAccessBarOnNonSeekableExtensionlessHTML(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>", "guide": "<!doctype html><html><body><p>g</p></body></html>", "notes": "plain notes"}, nil)
	env.srv.sites = nonSeekableStore{env.srv.sites}
	guide := env.get(barHost, "/guide")
	if !strings.HasPrefix(guide.Header().Get("Content-Type"), "text/html") || !strings.Contains(guide.Body.String(), "<body><spot-access-bar") {
		t.Fatalf("guide = %q %q", guide.Header().Get("Content-Type"), guide.Body.String())
	}
	if notes := env.get(barHost, "/notes"); notes.Body.String() != "plain notes" || !strings.HasPrefix(notes.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("notes = %q %q", notes.Header().Get("Content-Type"), notes.Body.String())
	}
}

// The bar is UTF-8, so a UTF-16 page is served as stored.
func TestAccessBarLeavesUTF16PagesAlone(t *testing.T) {
	env := newTypesEnv(t)
	page := "\xFF\xFE<\x00p\x00>\x00"
	env.mustDeploy("bar", map[string]string{"index.html": page}, nil)
	if rec := env.get(barHost, "/"); rec.Body.String() != page {
		t.Fatalf("UTF-16 page = %q, want it unchanged", rec.Body.String())
	}
}

// A failed If-Match outranks a matching If-None-Match, and only GET and HEAD
// may get a 304.
func TestAccessBarPreconditionOrder(t *testing.T) {
	env := typedDeckEnv(t)
	etag := env.get(deckHost, "/").Header().Get("ETag")
	if rec := env.get(deckHost, "/", "If-Match", `"stale"`, "If-None-Match", etag); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("typed stale If-Match = %d, want 412", rec.Code)
	}
	env.mustDeploy("bar", map[string]string{"index.html": "<h1>bar</h1>"}, nil)
	plainTag := env.get(barHost, "/").Header().Get("ETag")
	if rec := env.get(barHost, "/", "If-Match", `"stale"`, "If-None-Match", plainTag); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("plain stale If-Match = %d, want 412", rec.Code)
	}
	req := httptestRequest(http.MethodPost, barHost, "/")
	req.Header.Set("If-None-Match", plainTag)
	if rec := env.do(req); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("POST with matching If-None-Match = %d, want 412", rec.Code)
	}
}

func TestIfNoneMatchMatchesQuotedTags(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{`"a"`, true},
		{`W/"a"`, true},
		{`"b", W/"a"`, true},
		{`*`, true},
		{`"x,*,y"`, false},
		{`"b"`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := ifNoneMatchMatches(tc.header, `"a"`); got != tc.want {
			t.Errorf("ifNoneMatchMatches(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
