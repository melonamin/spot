package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSiteAdmin struct {
	owned     []OwnedSite
	all       []SiteRecord
	err       error
	deleteErr error
	deleted   []string
}

func (f *fakeSiteAdmin) SitesOwnedBy(_ context.Context, _ Identity) ([]OwnedSite, error) {
	return f.owned, f.err
}

func (f *fakeSiteAdmin) AllSites(_ context.Context) ([]SiteRecord, error) {
	return f.all, f.err
}

func (f *fakeSiteAdmin) DeleteSite(ctx context.Context, site string, _ Identity, purge func(context.Context) error) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if purge != nil {
		if err := purge(ctx); err != nil {
			return err
		}
	}
	f.deleted = append(f.deleted, site)
	return nil
}

type blockingDeleteAdmin struct {
	started chan struct{}
	release chan struct{}
}

func (a *blockingDeleteAdmin) SitesOwnedBy(context.Context, Identity) ([]OwnedSite, error) {
	return nil, nil
}

func (a *blockingDeleteAdmin) AllSites(context.Context) ([]SiteRecord, error) {
	return nil, nil
}

func (a *blockingDeleteAdmin) DeleteSite(ctx context.Context, _ string, _ Identity, purge func(context.Context) error) error {
	close(a.started)
	<-a.release
	if purge != nil {
		return purge(ctx)
	}
	return nil
}

type signalDeployAuth struct {
	called chan struct{}
}

func (a *signalDeployAuth) AuthorizeDeploy(context.Context, string, Identity) (DeployAuthorization, error) {
	select {
	case a.called <- struct{}{}:
	default:
	}
	return DeployAuthorization{Action: "update"}, nil
}

func (a *signalDeployAuth) RecordDeploy(context.Context, DeployAuditEvent) error {
	return nil
}

func writePolicy(t *testing.T, dir, site, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, site), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, site, accessFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubSiteStore serves just enough S3 for the delete purge: a bucket
// listing with the given keys, then per-object deletes.
func stubSiteStore(t *testing.T, keys ...string) (*SiteStore, *[]string) {
	t.Helper()
	removed := &[]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Has("location"):
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			var sb strings.Builder
			sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
				`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Name>spot-sites</Name><IsTruncated>false</IsTruncated>`)
			for _, key := range keys {
				sb.WriteString("<Contents><Key>" + key + "</Key><Size>1</Size></Contents>")
			}
			sb.WriteString("</ListBucketResult>")
			io.WriteString(w, sb.String())
		case r.Method == http.MethodDelete:
			*removed = append(*removed, strings.TrimPrefix(r.URL.Path, "/spot-sites/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(ts.Close)
	sites, err := NewSiteStore(strings.TrimPrefix(ts.URL, "http://"), "k", "s", "spot-sites")
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	return sites, removed
}

func sitesRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, "http://spot-api"+path, nil)
	req.Header.Set("X-Forwarded-Host", "spot.localhost")
	req.Header.Set("X-Forwarded-For", "100.64.0.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	return req
}

func TestMySitesListsOwnedWithPolicySummary(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "locked-site", `{"allow":["a@example.com","team-x"]}`)
	admin := &fakeSiteAdmin{owned: []OwnedSite{
		{SiteRecord: SiteRecord{Name: "open-site", CreatedAt: time.Now(), UpdatedAt: time.Now()},
			FileCount: 7, TotalBytes: 1024},
		{SiteRecord: SiteRecord{Name: "locked-site", CreatedAt: time.Now(), UpdatedAt: time.Now()},
			FileCount: 2, TotalBytes: 64},
	}}
	srv := &Server{
		siteAdmin:      admin,
		policies:       NewPolicyStore(dir, time.Minute),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("my sites = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []ownedSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 2 {
		t.Fatalf("sites = %+v, want 2 entries", body.Sites)
	}
	open, locked := body.Sites[0], body.Sites[1]
	if open.Name != "open-site" || open.Restricted || open.FileCount != 7 || open.TotalBytes != 1024 {
		t.Errorf("open site = %+v", open)
	}
	if open.URL != "https://open-site.spot.localhost/" {
		t.Errorf("open site url = %q", open.URL)
	}
	if locked.Name != "locked-site" || !locked.Restricted || locked.AllowCount != 2 {
		t.Errorf("locked site = %+v", locked)
	}
}

func TestSitesAPIIsApexOnly(t *testing.T) {
	srv := &Server{
		siteAdmin:      &fakeSiteAdmin{},
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api/sites/mine"},
		{http.MethodGet, "/api/sites/public"},
		{http.MethodDelete, "/api/sites/demo"},
	} {
		req := sitesRequest(tt.method, tt.path)
		req.Header.Set("X-Forwarded-Host", "demo.spot.localhost")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "platform root") {
			t.Errorf("%s %s from subdomain = %d %s, want 400 platform root",
				tt.method, tt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestSiteURLPreservesRequestPort(t *testing.T) {
	srv := &Server{spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t)}
	req := sitesRequest(http.MethodGet, "/api/sites/mine")
	req.Header.Set("X-Forwarded-Host", "spot.localhost:8080")
	req.Header.Set("X-Forwarded-Proto", "http")
	if got := srv.siteURL(req, "demo"); got != "http://demo.spot.localhost:8080/" {
		t.Fatalf("siteURL = %q, want request port preserved", got)
	}
}

func TestSitesAPIRequiresIdentity(t *testing.T) {
	sites, _ := stubSiteStore(t)
	srv := &Server{
		siteAdmin:      &fakeSiteAdmin{},
		sites:          sites,
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api/sites/mine"},
		{http.MethodGet, "/api/sites/public"},
		{http.MethodDelete, "/api/sites/demo"},
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(tt.method, tt.path))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without resolver = %d, want 503", tt.method, tt.path, rec.Code)
		}
	}
}

func TestPublicSitesFiltersRestrictedAndMarksYours(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "locked", `{"allow":["bob@example.com"]}`)
	writePolicy(t, dir, "broken", `{not json`)
	admin := &fakeSiteAdmin{all: []SiteRecord{
		{Name: "mine", OwnerEmail: "alice@example.com", OwnerName: "Alice"},
		{Name: "locked", OwnerEmail: "bob@example.com", OwnerName: "Bob"},
		{Name: "broken", OwnerEmail: "carol@example.com"},
		{Name: "theirs", OwnerEmail: "bob@example.com"},
	}}
	srv := &Server{
		siteAdmin:      admin,
		policies:       NewPolicyStore(dir, time.Minute),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/public"))
	if rec.Code != http.StatusOK {
		t.Fatalf("public sites = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []publicSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 2 {
		t.Fatalf("sites = %+v, want locked and broken filtered out", body.Sites)
	}
	mine, theirs := body.Sites[0], body.Sites[1]
	if mine.Name != "mine" || !mine.Yours || mine.Owner != "Alice" {
		t.Errorf("own site = %+v", mine)
	}
	// No owner name on record: the gallery falls back to the email.
	if theirs.Name != "theirs" || theirs.Yours || theirs.Owner != "bob@example.com" {
		t.Errorf("other site = %+v", theirs)
	}
}

func TestDeleteSiteValidation(t *testing.T) {
	sites, _ := stubSiteStore(t)
	srv := &Server{
		siteAdmin:      &fakeSiteAdmin{},
		sites:          sites,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/UPPER"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "site name") {
		t.Errorf("invalid name = %d %s, want 400 site name", rec.Code, rec.Body.String())
	}
}

func TestDeleteSiteAuthorizationOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		deleteErr  error
		wantCode   int
		wantStatus string // audited status, "" for no audit event
	}{
		{"not found", ErrSiteNotFound, http.StatusNotFound, ""},
		{"forbidden", ErrDeployForbidden, http.StatusForbidden, "denied"},
		{"registry failure", io.ErrUnexpectedEOF, http.StatusInternalServerError, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sites, _ := stubSiteStore(t)
			audit := &recordingDeployAuth{}
			srv := &Server{
				siteAdmin:      &fakeSiteAdmin{deleteErr: tt.deleteErr},
				deployAuth:     audit,
				sites:          sites,
				resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
				spotDomain:     "spot.localhost",
				trustedProxies: testTrustedProxies(t),
				deployLimit:    NewRateLimiter(1000, 1000),
			}
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
			if rec.Code != tt.wantCode {
				t.Fatalf("delete = %d %s, want %d", rec.Code, rec.Body.String(), tt.wantCode)
			}
			if tt.wantStatus == "" {
				if len(audit.events) != 0 {
					t.Fatalf("audit events = %+v, want none", audit.events)
				}
				return
			}
			if len(audit.events) != 1 {
				t.Fatalf("audit events = %d, want 1", len(audit.events))
			}
			event := audit.events[0]
			if event.Action != "delete" || event.Status != tt.wantStatus || event.Site != "demo" {
				t.Fatalf("audit event = %+v", event)
			}
		})
	}
}

func TestDeleteSitePurgesFilesAndAudits(t *testing.T) {
	sites, removed := stubSiteStore(t, "demo/index.html", "demo/css/app.css")
	admin := &fakeSiteAdmin{}
	audit := &recordingDeployAuth{}
	srv := &Server{
		siteAdmin:      admin,
		deployAuth:     audit,
		sites:          sites,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["site"] != "demo" || body["files"].(float64) != 2 {
		t.Errorf("delete response = %v", body)
	}
	if len(admin.deleted) != 1 || admin.deleted[0] != "demo" {
		t.Errorf("registry deletes = %v, want [demo]", admin.deleted)
	}
	if len(*removed) != 2 {
		t.Errorf("removed objects = %v, want both site files", *removed)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	event := audit.events[0]
	if event.Action != "delete" || event.Status != "success" || event.FileCount != 2 {
		t.Fatalf("audit event = %+v", event)
	}
}

func TestDeleteSitePurgeFailureLeavesSite(t *testing.T) {
	admin := &fakeSiteAdmin{}
	audit := &recordingDeployAuth{}
	srv := &Server{
		siteAdmin:      admin,
		deployAuth:     audit,
		sites:          failingSiteStore{},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete with failing purge = %d, want 500", rec.Code)
	}
	if len(admin.deleted) != 0 {
		t.Fatalf("registry deletes = %v, want none on purge failure", admin.deleted)
	}
	if len(audit.events) != 1 || audit.events[0].Status != "failed" {
		t.Fatalf("audit events = %+v, want one failed", audit.events)
	}
}

func TestDeleteSiteSerializesConcurrentDeploy(t *testing.T) {
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(context.Background(), "demo", "index.html", "text/html", []byte("old")); err != nil {
		t.Fatal(err)
	}
	admin := &blockingDeleteAdmin{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	auth := &signalDeployAuth{called: make(chan struct{}, 1)}
	srv := &Server{
		siteAdmin:      admin,
		deployAuth:     auth,
		sites:          sites,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	handler := srv.routes()

	deleteDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
		deleteDone <- rec.Code
	}()

	select {
	case <-admin.started:
	case <-time.After(time.Second):
		t.Fatal("delete did not start")
	}

	deployReq := deployRequest(t, "spot.localhost", "demo",
		map[string]string{"index.html": "new"})
	deployDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, deployReq)
		deployDone <- rec.Code
	}()

	select {
	case <-auth.called:
		t.Fatal("deploy authorized while delete still held the site mutation lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(admin.release)
	select {
	case code := <-deleteDone:
		if code != http.StatusOK {
			t.Fatalf("delete = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("delete did not finish")
	}
	select {
	case <-auth.called:
	case <-time.After(time.Second):
		t.Fatal("deploy did not authorize after delete finished")
	}
	select {
	case code := <-deployDone:
		if code != http.StatusOK {
			t.Fatalf("deploy = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("deploy did not finish")
	}
}

func TestSitePreviewServesScreenshotForOpenSitesOnly(t *testing.T) {
	dir := t.TempDir()
	png := "\x89PNG\r\n\x1a\nfake-png-body"
	writeSiteFile(t, dir, "shot", "_screenshot.png", png)
	writeSiteFile(t, dir, "locked", "_screenshot.png", png)
	writePolicy(t, dir, "locked", `{"allow":["bob@example.com"]}`)
	// An HTML file named like a screenshot would run script in the apex
	// origin if served as anything renderable.
	writeSiteFile(t, dir, "sneaky", "_screenshot.jpg", "<html><script>alert(1)</script></html>")

	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin:      &fakeSiteAdmin{},
		policies:       NewPolicyStore(dir, time.Minute),
		spotDomain:     "spot.localhost",
		sites:          sites,
		trustedProxies: testTrustedProxies(t),
	}

	t.Run("serves an open site's screenshot", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/shot/preview"))
		if rec.Code != http.StatusOK {
			t.Fatalf("preview = %d %s, want 200", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("content-type = %q, want image/png", ct)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("missing nosniff header")
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("cache-control = %q, want no-store", rec.Header().Get("Cache-Control"))
		}
		if rec.Body.String() != png {
			t.Errorf("served body did not match the stored screenshot")
		}
	})

	t.Run("404 when the site ships none", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/bare/preview"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("missing preview = %d, want 404", rec.Code)
		}
	})

	t.Run("404 for restricted sites so previews never leak", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/locked/preview"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("restricted preview = %d, want 404", rec.Code)
		}
	})

	t.Run("refuses a non-image masquerading as a screenshot", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/sneaky/preview"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("html screenshot = %d %s, want 404", rec.Code, rec.Body.String())
		}
	})
}

func TestPublicSitesIncludePreviewWhenScreenshotPresent(t *testing.T) {
	dir := t.TempDir()
	writeSiteFile(t, dir, "shot", "_screenshot.jpg", "\x89PNG\r\n\x1a\n...")
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	admin := &fakeSiteAdmin{all: []SiteRecord{
		{Name: "shot", OwnerEmail: "a@example.com"},
		{Name: "plain", OwnerEmail: "a@example.com"},
	}}
	srv := &Server{
		siteAdmin:      admin,
		policies:       NewPolicyStore(dir, time.Minute),
		resolver:       NewStaticResolver("a@example.com", "A", nil),
		spotDomain:     "spot.localhost",
		sites:          sites,
		trustedProxies: testTrustedProxies(t),
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/public"))
	if rec.Code != http.StatusOK {
		t.Fatalf("public sites = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []publicSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	preview := map[string]string{}
	for _, site := range body.Sites {
		preview[site.Name] = site.Preview
	}
	if preview["shot"] != "/api/sites/shot/preview" {
		t.Errorf("shot preview = %q, want /api/sites/shot/preview", preview["shot"])
	}
	if preview["plain"] != "" {
		t.Errorf("plain preview = %q, want empty", preview["plain"])
	}
}
