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
		sites:          newTestSiteStore(t), // unreachable: the purge must fail
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
