package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloudflareConfigStatus(t *testing.T) {
	for _, key := range []string{
		"SPOT_CLOUDFLARE_API_TOKEN",
		"SPOT_CLOUDFLARE_ACCOUNT_ID",
		"SPOT_CLOUDFLARE_ZONE_ID",
		"SPOT_CLOUDFLARE_BASE_DOMAIN",
		"SPOT_CLOUDFLARE_PROJECT_PREFIX",
	} {
		t.Setenv(key, "")
	}
	if got := loadCloudflareConfigFromEnv(); got.Status != cloudflareConfigDisabled {
		t.Fatalf("empty cloudflare config status = %q, want disabled", got.Status)
	}

	t.Setenv("SPOT_CLOUDFLARE_API_TOKEN", "token")
	t.Setenv("SPOT_CLOUDFLARE_ACCOUNT_ID", "acct")
	if got := loadCloudflareConfigFromEnv(); got.Status != cloudflareConfigPartial || len(got.Missing) != 2 {
		t.Fatalf("partial cloudflare config = %+v, want partial with two missing keys", got)
	}

	t.Setenv("SPOT_CLOUDFLARE_ZONE_ID", "zone")
	t.Setenv("SPOT_CLOUDFLARE_BASE_DOMAIN", "Pages.Example.Com.")
	if got := loadCloudflareConfigFromEnv(); got.Status != cloudflareConfigEnabled ||
		got.BaseDomain != "pages.example.com" || got.ProjectPrefix != defaultCloudflareProjectPrefix {
		t.Fatalf("enabled cloudflare config = %+v", got)
	}
}

func TestCloudflareEligibilityRejectsSpotRuntimeAndFunctions(t *testing.T) {
	snap := cloudflareSnapshot{Files: []cloudflareSiteFile{
		{Path: "index.html", Data: []byte(`<script src="/spot.js"></script><script>window.spot.db("x")</script>`)},
		{Path: accessFileName, Data: []byte(`{"allow":["a@example.com"]}`)},
		{Path: "functions/api.js", Data: []byte("export function onRequest() {}")},
		{Path: "_routes.json", Data: []byte("{}")},
	}}
	got := checkCloudflareEligibility(snap)
	if got.Eligible {
		t.Fatalf("eligibility = eligible, want rejected")
	}
	for _, want := range []string{
		accessFileName,
		"functions/",
		"_routes.json",
		"window.spot",
		"Spot's browser SDK",
	} {
		found := false
		for _, reason := range got.Reasons {
			if strings.Contains(reason, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("reasons = %v, want reason containing %q", got.Reasons, want)
		}
	}
}

type fakeCloudflareAPI struct {
	existingProject bool
	dnsRecords      []cloudflareDNSRecord
	fail            map[string]error
	calls           []string
	uploaded        []string
	deletedDNS      []string
}

func (f *fakeCloudflareAPI) err(op string) error {
	if f.fail == nil {
		return nil
	}
	return f.fail[op]
}

func (f *fakeCloudflareAPI) GetProject(context.Context, string, string) (*cloudflareProject, error) {
	f.calls = append(f.calls, "get-project")
	if err := f.err("get-project"); err != nil {
		return nil, err
	}
	if !f.existingProject {
		return nil, errCloudflareNotFound
	}
	return &cloudflareProject{Name: "spot-demo"}, nil
}

func (f *fakeCloudflareAPI) CreateProject(context.Context, string, string) error {
	f.calls = append(f.calls, "create-project")
	return f.err("create-project")
}

func (f *fakeCloudflareAPI) GetUploadToken(context.Context, string, string) (string, error) {
	f.calls = append(f.calls, "upload-token")
	return "upload-token", f.err("upload-token")
}

func (f *fakeCloudflareAPI) CheckMissing(_ context.Context, _ string, hashes []string) ([]string, error) {
	f.calls = append(f.calls, "check-missing")
	return hashes, f.err("check-missing")
}

func (f *fakeCloudflareAPI) UploadAssets(_ context.Context, _ string, files []cloudflareSiteFile) error {
	f.calls = append(f.calls, "upload-assets")
	for _, file := range files {
		f.uploaded = append(f.uploaded, file.Path)
	}
	return f.err("upload-assets")
}

func (f *fakeCloudflareAPI) UpsertHashes(context.Context, string, []string) error {
	f.calls = append(f.calls, "upsert-hashes")
	return f.err("upsert-hashes")
}

func (f *fakeCloudflareAPI) CreateDeployment(context.Context, string, string, map[string]string) (cloudflareDeployment, error) {
	f.calls = append(f.calls, "create-deployment")
	return cloudflareDeployment{ID: "dep-1", URL: "https://dep.pages.dev"}, f.err("create-deployment")
}

func (f *fakeCloudflareAPI) AddDomain(context.Context, string, string, string) error {
	f.calls = append(f.calls, "add-domain")
	return f.err("add-domain")
}

func (f *fakeCloudflareAPI) DeleteDomain(context.Context, string, string, string) error {
	f.calls = append(f.calls, "delete-domain")
	return f.err("delete-domain")
}

func (f *fakeCloudflareAPI) ListDNSRecords(context.Context, string, string) ([]cloudflareDNSRecord, error) {
	f.calls = append(f.calls, "list-dns")
	return f.dnsRecords, f.err("list-dns")
}

func (f *fakeCloudflareAPI) CreateDNSRecord(context.Context, string, string, string) error {
	f.calls = append(f.calls, "create-dns")
	return f.err("create-dns")
}

func (f *fakeCloudflareAPI) DeleteDNSRecord(_ context.Context, _, recordID string) error {
	f.calls = append(f.calls, "delete-dns")
	f.deletedDNS = append(f.deletedDNS, recordID)
	return f.err("delete-dns")
}

func (f *fakeCloudflareAPI) DeleteProject(context.Context, string, string) error {
	f.calls = append(f.calls, "delete-project")
	return f.err("delete-project")
}

func newCloudflareTestServer(t *testing.T, client cloudflareAPI) (*Server, *CloudflarePublicationStore) {
	t.Helper()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	actor := Identity{Email: "alice@example.com", PeerIP: "100.64.0.7", Name: "Alice"}
	if _, err := registry.AuthorizeDeploy(context.Background(), "demo", actor); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(context.Background(), "demo", "index.html", "text/html", []byte("<h1>demo</h1>")); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	cfg := cloudflareConfig{
		APIToken:      "token",
		AccountID:     "acct",
		ZoneID:        "zone",
		BaseDomain:    "pages.example.com",
		ProjectPrefix: "spot-",
		Status:        cloudflareConfigEnabled,
	}
	srv := &Server{
		siteAdmin:      registry,
		siteManager:    registry,
		sites:          sites,
		resolver:       NewStaticResolver(actor.Email, actor.Name, nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
		cloudflarePubs: repo,
		cloudflare:     &CloudflarePublisher{cfg: cfg, repo: repo, client: client},
	}
	return srv, repo
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openSQLiteDB(context.Background(), filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCloudflarePublishAPIHappyPath(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s, want 200", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.ProjectName != "spot-demo" || pub.Hostname != "demo.pages.example.com" ||
		pub.DeploymentID != "dep-1" || pub.Status != "published" {
		t.Fatalf("publication = %+v", pub)
	}
	for _, want := range []string{"create-project", "upload-token", "check-missing", "upload-assets", "upsert-hashes", "create-deployment", "add-domain", "create-dns"} {
		found := false
		for _, call := range cf.calls {
			if call == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("calls = %v, want %s", cf.calls, want)
		}
	}
}

func TestCloudflarePublishRequiresManager(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, _ := newCloudflareTestServer(t, cf)
	srv.resolver = NewStaticResolver("bob@example.com", "Bob", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("publish by non-owner = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if len(cf.calls) != 0 {
		t.Fatalf("cloudflare calls = %v, want none before auth", cf.calls)
	}
}

func TestCloudflarePublishRejectsUnmanagedProject(t *testing.T) {
	cf := &fakeCloudflareAPI{existingProject: true}
	srv, _ := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("publish unmanaged project = %d %s, want 502 already exists", rec.Code, rec.Body.String())
	}
	if len(cf.uploaded) != 0 {
		t.Fatalf("uploaded = %v, want none", cf.uploaded)
	}
}

func TestCloudflarePublishRecordsDNSConflictFailure(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "other", Type: "A", Name: "demo.pages.example.com", Content: "192.0.2.10",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "conflicting record") {
		t.Fatalf("publish DNS conflict = %d %s, want 502 conflict", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || !strings.Contains(pub.LastError, "conflicting record") {
		t.Fatalf("publication failure = %+v", pub)
	}
}

func TestCloudflareUnpublishRemovesDNSDomainProjectAndRow(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "dns-1", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site:        "demo",
		ProjectName: "spot-demo",
		Hostname:    "demo.pages.example.com",
		Status:      "published",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s, want 200", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub != nil {
		t.Fatalf("publication after unpublish = %+v, want nil", pub)
	}
	if len(cf.deletedDNS) != 1 || cf.deletedDNS[0] != "dns-1" {
		t.Fatalf("deleted DNS = %v, want dns-1", cf.deletedDNS)
	}
	for _, want := range []string{"delete-domain", "delete-project"} {
		found := false
		for _, call := range cf.calls {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("calls = %v, want %s", cf.calls, want)
		}
	}
}

func TestDeleteSiteWithCloudflarePublicationReturnsConflict(t *testing.T) {
	db := openTestDB(t)
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site:        "demo",
		ProjectName: "spot-demo",
		Hostname:    "demo.pages.example.com",
		Status:      "published",
	}); err != nil {
		t.Fatal(err)
	}
	admin := &fakeSiteAdmin{}
	srv := &Server{
		siteAdmin:      admin,
		sites:          listOnlySiteStore{},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
		cloudflarePubs: repo,
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete published site = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if len(admin.deleted) != 0 {
		t.Fatalf("deleted = %v, want none", admin.deleted)
	}
}

type listOnlySiteStore struct{}

func (listOnlySiteStore) Put(context.Context, string, string, string, []byte) error {
	return errors.New("unused")
}

func (listOnlySiteStore) List(context.Context, string) ([]string, error) {
	return []string{"index.html"}, nil
}

func (listOnlySiteStore) Open(context.Context, string, string) (io.ReadCloser, SiteFileInfo, error) {
	return nil, SiteFileInfo{}, errors.New("unused")
}

func (listOnlySiteStore) Remove(context.Context, string, string) error {
	return nil
}

func TestCloudflareClientUsesWranglerUploadEndpoints(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/accounts/acct/pages/projects/spot-demo/upload-token":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"jwt": "jwt"}})
		case "/pages/assets/check-missing":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []string{"hash"}})
		case "/pages/assets/upload", "/pages/assets/upsert-hashes", "/accounts/acct/pages/projects/spot-demo/domains", "/zones/zone/dns_records":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case "/accounts/acct/pages/projects/spot-demo/deployments":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "dep", "url": "https://dep.pages.dev"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	client := NewCloudflareClient("runtime-token")
	client.baseURL = ts.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	jwt, err := client.GetUploadToken(ctx, "acct", "spot-demo")
	if err != nil || jwt != "jwt" {
		t.Fatalf("upload token = %q, %v", jwt, err)
	}
	if _, err := client.CheckMissing(ctx, jwt, []string{"hash"}); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadAssets(ctx, jwt, []cloudflareSiteFile{{Path: "index.html", Hash: "hash", Data: []byte("x"), ContentType: "text/html"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.UpsertHashes(ctx, jwt, []string{"hash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateDeployment(ctx, "acct", "spot-demo", map[string]string{"/index.html": "hash"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /accounts/acct/pages/projects/spot-demo/upload-token",
		"POST /pages/assets/check-missing",
		"POST /pages/assets/upload",
		"POST /pages/assets/upsert-hashes",
		"POST /accounts/acct/pages/projects/spot-demo/deployments",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seen requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}
