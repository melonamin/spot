package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func accessRequest(site, email, body string) *http.Request {
	req := siteRequestWithBody(http.MethodPut, "sites.localhost:8443", "/api/sites/"+site+"/access", body)
	if email != "" {
		asForwardUser(req, email)
	}
	return req
}

func siteRequestWithBody(method, host, path, body string) *http.Request {
	req := httptest.NewRequest(method, "http://spot-api"+path, strings.NewReader(body))
	req.Header.Set("X-Forwarded-Host", host)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeAccessResponse(t *testing.T, body []byte) (allow []string, restricted bool) {
	t.Helper()
	var out struct {
		Site       string   `json:"site"`
		Allow      []string `json:"allow"`
		Restricted bool     `json:"restricted"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Allow, out.Restricted
}

func TestSiteAccessUpdate(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "demo.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "demo",
		`{"allow":["owner@example.com"],"maintainers":["maint@example.com"]}`)

	if rec := st.do(siteRequest(http.MethodGet, host, "/")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private site anonymous = %d, want 401", rec.Code)
	}

	// Denied paths: anonymous, stranger, invalid policy, unknown site, a
	// maintainer changing maintainers, and site-host callers.
	if rec := st.do(accessRequest("demo", "", `{}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous access change = %d, want 404 (no identity)", rec.Code)
	}
	if rec := st.do(accessRequest("demo", "stranger@example.com", `{}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger access change = %d, want 403", rec.Code)
	}
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":"everyone"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy = %d, want 400", rec.Code)
	}
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":[]} {}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d, want 400", rec.Code)
	}
	if rec := st.do(accessRequest("missing", "owner@example.com", `{}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown site = %d, want 404", rec.Code)
	}
	if rec := st.do(accessRequest("demo", "maint@example.com", `{"allow":["platform"]}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("maintainer dropping maintainers = %d, want 403", rec.Code)
	}
	onSite := asForwardUser(siteRequestWithBody(http.MethodPut, host, "/api/sites/demo/access", `{}`), "owner@example.com")
	if rec := st.do(onSite); rec.Code != http.StatusBadRequest {
		t.Fatalf("access change from a site host = %d, want 400", rec.Code)
	}

	// A maintainer may change who can view the site.
	rec := st.do(accessRequest("demo", "maint@example.com", `{"allow":["platform"],"maintainers":["maint@example.com"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("maintainer allow change = %d %s", rec.Code, rec.Body.String())
	}
	if allow, restricted := decodeAccessResponse(t, rec.Body.Bytes()); !restricted || len(allow) != 1 || allow[0] != "platform" {
		t.Fatalf("maintainer response allow=%v restricted=%v", allow, restricted)
	}

	// The owner makes the site public; an anonymous visitor can then load it.
	rec = st.do(accessRequest("demo", "owner@example.com", `{}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"allow":null`) {
		t.Fatalf("owner public = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(siteRequest(http.MethodGet, host, "/")); rec.Code != http.StatusOK {
		t.Fatalf("public site anonymous = %d, want 200", rec.Code)
	}

	// Deny-all serializes as an empty list, not null.
	rec = st.do(accessRequest("demo", "owner@example.com", `{"allow":[]}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"allow":[]`) {
		t.Fatalf("deny-all = %d %s", rec.Code, rec.Body.String())
	}

	// Access-only audit rows must not hide the deployed content summary.
	mine := st.do(asForwardUser(sitesRequestFor("/api/sites/mine"), "owner@example.com"))
	var listing struct {
		Sites []ownedSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(mine.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Sites) != 1 || listing.Sites[0].FileCount != 2 || listing.Sites[0].LastDeploy == nil ||
		listing.Sites[0].Allow == nil || len(listing.Sites[0].Allow) != 0 {
		t.Fatalf("mine after access change = %+v", listing.Sites)
	}
}

func TestSiteAccessGenerationChangeIsConflict(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", "")
	st.srv.siteAdmin = staleGenerationAdmin{st.registry}
	rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":["platform"]}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale generation = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if pending, err := st.registry.HasPendingPolicyTransition(context.Background(), "demo"); err != nil || pending {
		t.Fatalf("pending transition after conflict = %v, %v", pending, err)
	}
}

type staleGenerationAdmin struct{ *SiteRegistry }

func (a staleGenerationAdmin) SiteContentGeneration(ctx context.Context, site string) (int64, error) {
	generation, err := a.SiteRegistry.SiteContentGeneration(ctx, site)
	return generation - 1, err
}

func sitesRequestFor(path string) *http.Request {
	return siteRequest(http.MethodGet, "sites.localhost:8443", path)
}

func TestVisibleSites(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open", "")
	st.deploy(t, "owner@example.com", "platform", `{"allow":["platform"]}`)
	st.deploy(t, "owner@example.com", "private", `{"allow":["owner@example.com"]}`)
	st.deploy(t, "owner@example.com", "delegated", `{"allow":["owner@example.com"],"maintainers":["viewer@example.com"]}`)
	st.deploy(t, "viewer@example.com", "own", `{"allow":["viewer@example.com"]}`)

	visible := func(email string, groups string) map[string]visibleSiteJSON {
		t.Helper()
		req := asForwardUser(sitesRequestFor("/api/sites/visible"), email)
		req.Header.Set("Remote-Groups", groups)
		rec := st.do(req)
		if rec.Code != http.StatusOK {
			t.Fatalf("visible = %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Sites []visibleSiteJSON `json:"sites"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]visibleSiteJSON{}
		for _, site := range body.Sites {
			out[site.Name] = site
		}
		return out
	}

	got := visible("viewer@example.com", "platform")
	for _, name := range []string{"open", "platform", "delegated", "own"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("viewer missing %s in %v", name, got)
		}
	}
	if _, ok := got["private"]; ok {
		t.Fatal("viewer sees another user's private site")
	}
	if got["open"].Restricted || got["open"].Allow != nil || got["open"].Yours {
		t.Fatalf("open entry = %+v", got["open"])
	}
	if !got["platform"].Restricted || len(got["platform"].Allow) != 1 || got["platform"].Allow[0] != "platform" {
		t.Fatalf("platform entry = %+v", got["platform"])
	}
	if !got["own"].Yours || got["own"].URL != "http://own.sites.localhost:8443/" {
		t.Fatalf("own entry = %+v", got["own"])
	}

	outsider := visible("outsider@example.com", "")
	if len(outsider) != 1 || outsider["open"].Name != "open" {
		t.Fatalf("outsider sees %v, want only the open site", outsider)
	}

	if rec := st.do(sitesRequestFor("/api/sites/visible")); rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous visible = %d, want 404 (no identity)", rec.Code)
	}
}

func TestManageableSitesIncludeAllow(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open", "")
	st.deploy(t, "owner@example.com", "locked", `{"allow":["owner@example.com","platform"]}`)
	rec := st.do(asForwardUser(sitesRequestFor("/api/sites/manageable"), "owner@example.com"))
	var body struct {
		Sites []struct {
			Name  string    `json:"name"`
			Allow *[]string `json:"allow"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	allow := map[string]*[]string{}
	for _, site := range body.Sites {
		allow[site.Name] = site.Allow
	}
	if allow["open"] != nil || allow["locked"] == nil || len(*allow["locked"]) != 2 {
		t.Fatalf("manageable allow = %s", rec.Body.String())
	}
}
