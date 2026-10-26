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

	// Denied paths: anonymous, stranger, invalid policy, unknown site, and
	// site-host callers.
	if rec := st.do(accessRequest("demo", "", `{}`)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous access change = %d, want 401 (no identity)", rec.Code)
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
	// A viewer does not learn who else may view a site; its managers do.
	if !got["platform"].Restricted || got["platform"].Allow != nil {
		t.Fatalf("platform entry for a viewer = %+v, want no allowlist", got["platform"])
	}
	if allow := got["delegated"].Allow; len(allow) != 1 || allow[0] != "owner@example.com" {
		t.Fatalf("delegated entry for its maintainer = %+v", got["delegated"])
	}
	if allow := got["own"].Allow; len(allow) != 1 || allow[0] != "viewer@example.com" {
		t.Fatalf("own entry = %+v", got["own"])
	}
	if allow := visible("owner@example.com", "")["platform"].Allow; len(allow) != 1 || allow[0] != "platform" {
		t.Fatalf("platform entry for its owner allow = %v", allow)
	}
	// The owner's email goes to viewers a site was shared with, not to every
	// viewer of an open site.
	if got["platform"].OwnerEmail != "owner@example.com" {
		t.Fatalf("platform owner_email = %q", got["platform"].OwnerEmail)
	}
	if got["open"].OwnerEmail != "" || got["open"].Owner == "" {
		t.Fatalf("open entry for another user = %+v, want owner name without email", got["open"])
	}
	if email := visible("owner@example.com", "")["open"].OwnerEmail; email != "owner@example.com" {
		t.Fatalf("open entry for its owner owner_email = %q", email)
	}
	if !got["own"].Yours || got["own"].URL != "http://own.sites.localhost:8443/" {
		t.Fatalf("own entry = %+v", got["own"])
	}

	outsider := visible("outsider@example.com", "")
	if len(outsider) != 1 || outsider["open"].Name != "open" {
		t.Fatalf("outsider sees %v, want only the open site", outsider)
	}

	if rec := st.do(sitesRequestFor("/api/sites/visible")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous visible = %d, want 401 (no identity)", rec.Code)
	}
}

func TestManageableSitesIncludeAllow(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open", "")
	st.deploy(t, "owner@example.com", "locked", `{"allow":["owner@example.com","platform"]}`)
	rec := st.do(asForwardUser(sitesRequestFor("/api/sites/manageable"), "owner@example.com"))
	var body struct {
		Sites []struct {
			Name       string    `json:"name"`
			OwnerEmail string    `json:"owner_email"`
			Allow      *[]string `json:"allow"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	allow := map[string]*[]string{}
	for _, site := range body.Sites {
		allow[site.Name] = site.Allow
		if site.OwnerEmail != "owner@example.com" {
			t.Fatalf("manageable %s owner_email = %q", site.Name, site.OwnerEmail)
		}
	}
	if allow["open"] != nil || allow["locked"] == nil || len(*allow["locked"]) != 2 {
		t.Fatalf("manageable allow = %s", rec.Body.String())
	}
}

func accessAuditCount(t *testing.T, st *loginTestStack, site, actor, status string) int {
	t.Helper()
	var n int
	if err := st.registry.db.QueryRow(`SELECT count(*) FROM site_deploy_audit
		WHERE site = ? AND actor_email = ? AND action = 'access' AND status = ?`, site, actor, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A caller who cannot manage the site gets 403 and a denied audit row even
// while a policy transition is pending; only the owner or an admin reaches
// the reconcile and its 503.
func TestSiteAccessAuthorizesBeforeReconcile(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"],"maintainers":["maint@example.com"]}`)
	ctx := context.Background()
	generation, err := st.registry.SiteContentGeneration(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// The stored policy matches neither side, so the transition cannot be
	// reconciled automatically.
	if err := st.registry.BeginPolicyTransition(ctx, "demo", generation, absentPolicyHash, "sha256:next"); err != nil {
		t.Fatal(err)
	}

	rec := st.do(accessRequest("demo", "stranger@example.com", `{}`))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "recovery") {
		t.Fatalf("stranger during pending transition = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if got := accessAuditCount(t, st, "demo", "stranger@example.com", "denied"); got != 1 {
		t.Fatalf("stranger denied audit rows = %d, want 1", got)
	}
	if rec := st.do(accessRequest("demo", "owner@example.com", `{}`)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("owner during unresolvable transition = %d %s, want 503", rec.Code, rec.Body.String())
	}
	// The pending transition hides the maintainers list, so a maintainer is
	// indistinguishable from a stranger until the owner or an admin recovers.
	if rec := st.do(accessRequest("demo", "maint@example.com", `{"allow":["owner@example.com"],"maintainers":["maint@example.com"]}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("maintainer during pending transition = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if got := accessAuditCount(t, st, "demo", "owner@example.com", "denied"); got != 0 {
		t.Fatalf("owner denied audit rows = %d, want 0", got)
	}
}

// Maintainers compare as a set: a maintainer may reorder or re-case the list
// but not add, remove or replace an entry.
// Like a deploy, the access API lets a maintainer change the maintainers
// list, even to remove itself; the immutable owner keeps its claim.
func TestSiteAccessMaintainersMayChangeMaintainers(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo",
		`{"allow":["owner@example.com"],"maintainers":["maint@example.com","ops"]}`)

	if rec := st.do(accessRequest("demo", "maint@example.com", `{"allow":["platform"],"maintainers":["ops","extra@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("maintainer replacing maintainers = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// Denied: the removed maintainer.
	if rec := st.do(accessRequest("demo", "maint@example.com", `{}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("removed maintainer = %d %s, want 403", rec.Code, rec.Body.String())
	}
	// Allowed: the new maintainer, and the owner after every maintainer is gone.
	if rec := st.do(accessRequest("demo", "extra@example.com", `{"allow":["platform"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("new maintainer = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":["owner@example.com"],"maintainers":["ops"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("owner recovery = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMaintainersChangedIgnoresOrderCaseAndBlanks(t *testing.T) {
	policy := func(entries ...string) *AccessPolicy { return &AccessPolicy{Maintainers: entries} }
	for _, tt := range []struct {
		name          string
		current, next *AccessPolicy
		want          bool
	}{
		{"reordered", policy("a@x.com", "ops"), policy("ops", "a@x.com"), false},
		{"case and space", policy("a@x.com"), policy(" A@X.com "), false},
		{"duplicate and blank", policy("a@x.com"), policy("a@x.com", "a@x.com", " "), false},
		{"both empty", nil, policy(), false},
		{"added", policy("a@x.com"), policy("a@x.com", "b@x.com"), true},
		{"removed", policy("a@x.com", "b@x.com"), policy("a@x.com"), true},
		{"replaced same size", policy("a@x.com", "ops"), policy("a@x.com", "admins"), true},
		{"cleared", policy("a@x.com"), nil, true},
	} {
		if got := maintainersChanged(tt.current, tt.next); got != tt.want {
			t.Errorf("%s: maintainersChanged = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// When the policy write lands but its transition cannot be cleared, the new
// policy may be in force, so live realtime sessions must end at once: a retry
// finds nothing left to narrow and would never revoke them.
func TestSiteAccessUnresolvedCommitRevokesRealtime(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", "")
	st.srv.hub = NewHub()
	revoked := make(chan struct{})
	if !st.srv.hub.RegisterSession("demo", "viewer", st.srv.hub.SiteEpoch("demo"), make(chan Event, 1), func() { close(revoked) }) {
		t.Fatal("register session")
	}
	st.srv.deployAuth = &clearFailingRegistry{SiteRegistry: st.registry}

	rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":["owner@example.com"]}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("access change with lost transition clear = %d %s, want 500", rec.Code, rec.Body.String())
	}
	select {
	case <-revoked:
	default:
		t.Fatal("realtime session survived an unresolved policy commit")
	}
}

func TestDeployUnresolvedRestrictiveStagingRevokesRealtime(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", "")
	st.srv.hub = NewHub()
	revoked := make(chan struct{})
	if !st.srv.hub.RegisterSession("demo", "viewer", st.srv.hub.SiteEpoch("demo"), make(chan Event, 1), func() { close(revoked) }) {
		t.Fatal("register session")
	}
	st.srv.deployAuth = &clearFailingRegistry{SiteRegistry: st.registry}

	rec := st.do(asForwardUser(deployRequest(t, "sites.localhost:8443", "demo", map[string]string{
		"index.html": "<h1>demo</h1>", accessFileName: `{"allow":["owner@example.com"]}`,
	}), "owner@example.com"))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "fail-closed policy") {
		t.Fatalf("restrictive deploy with lost transition clear = %d %s, want 500", rec.Code, rec.Body.String())
	}
	select {
	case <-revoked:
	default:
		t.Fatal("realtime session survived an unresolved restrictive staging commit")
	}
}

// Access set through the API lives only in storage, so a redeploy of a bundle
// without _access.json must not silently reopen the site.
func TestRedeployKeepsAccessSetThroughTheAPI(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "demo.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "demo", "")
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":["owner@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("access change = %d %s", rec.Code, rec.Body.String())
	}

	st.deploy(t, "owner@example.com", "demo", "")
	if rec := st.do(siteRequest(http.MethodGet, host, "/")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous after redeploy without %s = %d, want 401", accessFileName, rec.Code)
	}

	// An explicit opt-out still gives the bundle sync semantics.
	rec := st.do(asForwardUser(accessRemovingDeployRequest(t, "sites.localhost:8443", "demo",
		map[string]string{"index.html": "<h1>demo</h1>"}), "owner@example.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("access-removing deploy = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(siteRequest(http.MethodGet, host, "/")); rec.Code != http.StatusOK {
		t.Fatalf("anonymous after preserve_access=false = %d, want 200", rec.Code)
	}
}

// preserve_access=false removes a stored policy, so only recognized values
// count; anything else is refused before the site changes.
func TestDeployPreserveAccessIsStrict(t *testing.T) {
	st := newLoginTestStack(t)
	const host = "demo.sites.localhost:8443"
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"]}`)
	for _, value := range []string{"", "keep", "ture", "2"} {
		req := deployRequestOrderedFields(t, "sites.localhost:8443", "demo",
			[][2]string{{"index.html", "<h1>demo</h1>"}}, map[string]string{"preserve_access": value})
		if rec := st.do(asForwardUser(req, "owner@example.com")); rec.Code != http.StatusBadRequest {
			t.Fatalf("preserve_access=%q = %d %s, want 400", value, rec.Code, rec.Body.String())
		}
		if rec := st.do(siteRequest(http.MethodGet, host, "/")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("site after preserve_access=%q = %d, want still restricted", value, rec.Code)
		}
	}
	for _, value := range []string{"true", "1", "YES", "on"} {
		req := deployRequestOrderedFields(t, "sites.localhost:8443", "demo",
			[][2]string{{"index.html", "<h1>demo</h1>"}}, map[string]string{"preserve_access": value})
		if rec := st.do(asForwardUser(req, "owner@example.com")); rec.Code != http.StatusOK {
			t.Fatalf("preserve_access=%q = %d %s, want 200", value, rec.Code, rec.Body.String())
		}
	}
}

type accessReadBody struct {
	Site       string          `json:"site"`
	Version    string          `json:"version"`
	Readable   bool            `json:"readable"`
	Restricted bool            `json:"restricted"`
	Pending    bool            `json:"pending"`
	Policy     json.RawMessage `json:"policy"`
	Error      string          `json:"error"`
}

func readAccess(t *testing.T, st *loginTestStack, site, email string) (*httptest.ResponseRecorder, accessReadBody) {
	t.Helper()
	req := siteRequest(http.MethodGet, "sites.localhost:8443", "/api/sites/"+site+"/access")
	if email != "" {
		asForwardUser(req, email)
	}
	rec := st.do(req)
	var body accessReadBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got := rec.Header().Get("ETag"); got == "" || got != body.Version {
			t.Fatalf("ETag %q, body version %q", got, body.Version)
		}
	}
	return rec, body
}

func conditionalAccessRequest(site, email, version, body string) *http.Request {
	req := accessRequest(site, email, body)
	req.Header.Set("If-Match", version)
	return req
}

func TestSiteAccessRead(t *testing.T) {
	st := newLoginTestStack(t)
	st.registry.admins = &AccessPolicy{Allow: []string{"admin@example.com"}, restrictAccess: true}
	const stored = `{"allow":["owner@example.com","platform"],"maintainers":["maint@example.com"],"ai":"visitors","download":false}`
	st.deploy(t, "owner@example.com", "demo", stored)
	st.deploy(t, "owner@example.com", "open", "")

	// Denied paths: anonymous, a visitor who may view but not manage, an
	// unknown site, and a site-host caller.
	if rec, _ := readAccess(t, st, "demo", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read = %d, want 401", rec.Code)
	}
	if rec, _ := readAccess(t, st, "demo", "viewer@example.com"); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "maint@") {
		t.Fatalf("visitor read = %d %s, want 403 without the policy", rec.Code, rec.Body.String())
	}
	if rec, _ := readAccess(t, st, "missing", "owner@example.com"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown site read = %d, want 404", rec.Code)
	}
	onSite := asForwardUser(siteRequest(http.MethodGet, "demo.sites.localhost:8443", "/api/sites/demo/access"), "owner@example.com")
	if rec := st.do(onSite); rec.Code != http.StatusBadRequest {
		t.Fatalf("read from a site host = %d, want 400", rec.Code)
	}

	// Allowed: owner, maintainer and admin read every field, so a client can
	// write back the policy it read without losing ai or download.
	for _, who := range []string{"owner@example.com", "maint@example.com", "admin@example.com"} {
		rec, body := readAccess(t, st, "demo", who)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s read = %d %s", who, rec.Code, rec.Body.String())
		}
		var policy map[string]any
		if err := json.Unmarshal(body.Policy, &policy); err != nil {
			t.Fatal(err)
		}
		if !body.Readable || !body.Restricted || body.Pending || body.Site != "demo" ||
			policy["ai"] != "visitors" || policy["download"] != false ||
			len(policy["allow"].([]any)) != 2 || len(policy["maintainers"].([]any)) != 1 {
			t.Fatalf("%s read = %+v policy %v", who, body, policy)
		}
		if want := accessPolicyVersion([]byte(stored), false); body.Version != want {
			t.Fatalf("version = %s, want %s", body.Version, want)
		}
	}

	// A site without a policy file is open and versioned as absent.
	if rec, body := readAccess(t, st, "open", "owner@example.com"); rec.Code != http.StatusOK ||
		body.Version != `"absent"` || body.Restricted || string(body.Policy) != "{}" {
		t.Fatalf("open read = %d %+v", rec.Code, body)
	}

	// Deny-all keeps its empty allow list; an open policy has none.
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":[],"maintainers":["maint@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("deny-all = %d %s", rec.Code, rec.Body.String())
	}
	if _, body := readAccess(t, st, "demo", "owner@example.com"); !body.Restricted ||
		string(body.Policy) != `{"allow":[],"maintainers":["maint@example.com"]}` {
		t.Fatalf("deny-all read = %+v %s", body, body.Policy)
	}
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"maintainers":["maint@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("open = %d %s", rec.Code, rec.Body.String())
	}
	if _, body := readAccess(t, st, "demo", "owner@example.com"); body.Restricted ||
		string(body.Policy) != `{"maintainers":["maint@example.com"]}` {
		t.Fatalf("open policy read = %+v %s", body, body.Policy)
	}

	// A deleted site has no access to manage.
	if rec := st.do(asForwardUser(siteRequest(http.MethodDelete, "sites.localhost:8443", "/api/sites/open"), "owner@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := readAccess(t, st, "open", "owner@example.com"); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted site read = %d, want 404", rec.Code)
	}
}

func TestSiteAccessIfMatch(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"],"maintainers":["maint@example.com"]}`)
	_, read := readAccess(t, st, "demo", "owner@example.com")

	// A change conditioned on the version read succeeds and returns the new one.
	next := `{"allow":["owner@example.com","platform"],"maintainers":["maint@example.com"]}`
	rec := st.do(conditionalAccessRequest("demo", "owner@example.com", read.Version, next))
	if rec.Code != http.StatusOK {
		t.Fatalf("conditional change = %d %s", rec.Code, rec.Body.String())
	}
	nextVersion := accessPolicyVersion([]byte(next), false)
	if rec.Header().Get("ETag") != nextVersion || !strings.Contains(rec.Body.String(), `"version":"\"sha256:`) {
		t.Fatalf("change ETag = %q body %s, want %s", rec.Header().Get("ETag"), rec.Body.String(), nextVersion)
	}

	// A second editor still holding the first version is refused, told the
	// current version, and the stored policy is untouched.
	rec = st.do(conditionalAccessRequest("demo", "maint@example.com", read.Version, `{}`))
	if rec.Code != http.StatusPreconditionFailed || rec.Header().Get("ETag") != nextVersion {
		t.Fatalf("stale change = %d %s etag %q, want 412", rec.Code, rec.Body.String(), rec.Header().Get("ETag"))
	}
	if _, body := readAccess(t, st, "demo", "owner@example.com"); body.Version != nextVersion {
		t.Fatalf("version after refused change = %s, want %s", body.Version, nextVersion)
	}
	if got := accessAuditCount(t, st, "demo", "maint@example.com", "success"); got != 0 {
		t.Fatalf("refused change audited as success %d times", got)
	}
	if rec := st.do(siteRequest(http.MethodGet, "demo.sites.localhost:8443", "/")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("site after refused opening change = %d, want still restricted", rec.Code)
	}

	// A deploy that ships a new policy also invalidates an earlier read.
	_, read = readAccess(t, st, "demo", "owner@example.com")
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"]}`)
	if rec := st.do(conditionalAccessRequest("demo", "owner@example.com", read.Version, `{}`)); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("change after a policy deploy = %d %s, want 412", rec.Code, rec.Body.String())
	}

	// A stranger learns nothing from a precondition: authorization comes first.
	if rec := st.do(conditionalAccessRequest("demo", "stranger@example.com", `"absent"`, `{}`)); rec.Code != http.StatusForbidden || rec.Header().Get("ETag") != "" {
		t.Fatalf("stranger conditional change = %d etag %q, want 403 without a version", rec.Code, rec.Header().Get("ETag"))
	}

	// Only one strong version is a precondition; "*", weak and listed tags
	// are refused before anything changes.
	for _, header := range []string{`*`, `W/` + nextVersion, nextVersion + `, "absent"`, `sha256:abc`} {
		if rec := st.do(conditionalAccessRequest("demo", "owner@example.com", header, `{}`)); rec.Code != http.StatusBadRequest {
			t.Fatalf("If-Match %s = %d %s, want 400", header, rec.Code, rec.Body.String())
		}
	}

	// Without If-Match the change is unconditional, as before.
	if rec := st.do(accessRequest("demo", "owner@example.com", `{}`)); rec.Code != http.StatusOK {
		t.Fatalf("unconditional change = %d %s", rec.Code, rec.Body.String())
	}

	// A site without a policy file matches the absent version.
	st.deploy(t, "owner@example.com", "open", "")
	if rec := st.do(conditionalAccessRequest("open", "owner@example.com", `"absent"`, `{"allow":["owner@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("change from absent = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(conditionalAccessRequest("open", "owner@example.com", `"absent"`, `{}`)); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("second change from absent = %d %s, want 412", rec.Code, rec.Body.String())
	}
}

// An unparsable stored policy fails closed for visitors; the access API
// reports it without echoing the file and lets a manager replace it.
func TestSiteAccessReadUnreadablePolicy(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"]}`)
	ctx := context.Background()
	broken := []byte(`{"allow":"secret@example.com"}`)
	if err := st.srv.sites.Put(ctx, "demo", accessFileName, "application/json", broken); err != nil {
		t.Fatal(err)
	}
	st.srv.policies.Invalidate("demo")

	rec, body := readAccess(t, st, "demo", "owner@example.com")
	if rec.Code != http.StatusOK || body.Readable || !body.Restricted || string(body.Policy) != "null" ||
		body.Version != accessPolicyVersion(broken, false) || strings.Contains(rec.Body.String(), "secret@") {
		t.Fatalf("unreadable read = %d %s", rec.Code, rec.Body.String())
	}

	mine := st.do(asForwardUser(sitesRequestFor("/api/sites/mine"), "owner@example.com"))
	manageable := st.do(asForwardUser(sitesRequestFor("/api/sites/manageable"), "owner@example.com"))
	for name, listing := range map[string]*httptest.ResponseRecorder{"mine": mine, "manageable": manageable} {
		if listing.Code != http.StatusOK || !strings.Contains(listing.Body.String(), `"policy_unreadable":true`) {
			t.Fatalf("%s listing = %d %s, want policy_unreadable", name, listing.Code, listing.Body.String())
		}
	}

	if rec := st.do(conditionalAccessRequest("demo", "owner@example.com", body.Version, `{"allow":["owner@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("repair = %d %s", rec.Code, rec.Body.String())
	}
	if _, body := readAccess(t, st, "demo", "owner@example.com"); !body.Readable {
		t.Fatalf("after repair = %+v", body)
	}
	if listing := st.do(asForwardUser(sitesRequestFor("/api/sites/mine"), "owner@example.com")); strings.Contains(listing.Body.String(), "policy_unreadable") {
		t.Fatalf("listing after repair = %s", listing.Body.String())
	}
}

// While a policy transition is pending the maintainers list is unknown, so
// only the owner or an admin may read the policy, and it is marked pending.
func TestSiteAccessReadDuringPendingTransition(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", `{"allow":["owner@example.com"],"maintainers":["maint@example.com"]}`)
	ctx := context.Background()
	generation, err := st.registry.SiteContentGeneration(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.registry.BeginPolicyTransition(ctx, "demo", generation, absentPolicyHash, "sha256:next"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"maint@example.com", "stranger@example.com"} {
		if rec, _ := readAccess(t, st, "demo", who); rec.Code != http.StatusForbidden {
			t.Fatalf("%s read during transition = %d %s, want 403", who, rec.Code, rec.Body.String())
		}
	}
	if rec, body := readAccess(t, st, "demo", "owner@example.com"); rec.Code != http.StatusOK || !body.Pending {
		t.Fatalf("owner read during transition = %d %s, want pending", rec.Code, rec.Body.String())
	}
}

// An access change records how the visitor and maintainer lists changed, so
// the audit log is reviewable without the stored file.
func TestSiteAccessAuditSummary(t *testing.T) {
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "demo", "")
	if rec := st.do(accessRequest("demo", "owner@example.com", `{"allow":["owner@example.com","platform"],"maintainers":["maint@example.com"]}`)); rec.Code != http.StatusOK {
		t.Fatalf("access change = %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do(accessRequest("demo", "maint@example.com", `{"allow":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("maintainer change = %d %s", rec.Code, rec.Body.String())
	}
	rows, err := st.registry.db.Query(`SELECT message FROM site_deploy_audit
		WHERE site = 'demo' AND action = 'access' AND status = 'success' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatal(err)
		}
		got = append(got, message)
	}
	want := []string{
		"visitors none -> 2; maintainers none -> 1",
		"visitors 2 -> 0; maintainers 1 -> none",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit messages = %q, want %q", got, want)
	}
}
