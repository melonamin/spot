package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		"SPOT_CLOUDFLARE_ACCESS_IDP_ID",
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
	t.Setenv("SPOT_CLOUDFLARE_ACCESS_IDP_ID", "otp-idp")
	if got := loadCloudflareConfigFromEnv(); got.Status != cloudflareConfigEnabled ||
		got.BaseDomain != "pages.example.com" || got.ProjectPrefix != defaultCloudflareProjectPrefix || !got.AccessEnabled() {
		t.Fatalf("enabled cloudflare config = %+v", got)
	}
}

func TestCloudflareProjectNameValidation(t *testing.T) {
	cfg := cloudflareConfig{ProjectPrefix: defaultCloudflareProjectPrefix}
	if got := cfg.ProjectName("demo"); got != "spot-demo" {
		t.Fatalf("short project name = %q, want spot-demo", got)
	}

	for _, siteLength := range []int{53, 54, 57, 58, 63} {
		site := strings.Repeat("a", siteLength-1) + "b"
		got := cfg.ProjectName(site)
		if len(got) > maxCloudflareProjectNameLength {
			t.Errorf("ProjectName(%d-byte site) length = %d, want <= %d: %q",
				siteLength, len(got), maxCloudflareProjectNameLength, got)
		}
		if !siteNameRe.MatchString(got) {
			t.Errorf("ProjectName(%d-byte site) = %q, want provider-safe DNS label", siteLength, got)
		}
		if siteLength == 53 && got != defaultCloudflareProjectPrefix+site {
			t.Errorf("ProjectName(%d-byte site) = %q, want unchanged boundary name", siteLength, got)
		}
		if siteLength > 53 && got == defaultCloudflareProjectPrefix+site {
			t.Errorf("ProjectName(%d-byte site) was not shortened", siteLength)
		}
	}

	longA := strings.Repeat("a", 62) + "a"
	longB := strings.Repeat("a", 62) + "b"
	if cfg.ProjectName(longA) == cfg.ProjectName(longB) {
		t.Fatal("distinct long site names produced the same project name")
	}
	if got, want := (cloudflareConfig{ProjectPrefix: " TEAM__Sites / "}).ProjectName("demo"), "team-sites-demo"; got != want {
		t.Errorf("normalized custom prefix project name = %q, want %q", got, want)
	}
	oversized := cloudflareConfig{ProjectPrefix: strings.Repeat("prefix-", 12)}
	if got := oversized.ProjectName("demo"); len(got) != maxCloudflareProjectNameLength || !siteNameRe.MatchString(got) {
		t.Errorf("oversized custom prefix project name = %q (length %d), want valid %d-byte name",
			got, len(got), maxCloudflareProjectNameLength)
	}
}

func TestCloudflareEligibilityRejectsSpotRuntimeAndFunctions(t *testing.T) {
	snap := cloudflareSnapshot{Files: []cloudflareSiteFile{
		{Path: "index.html", Data: []byte(`<script src="/spot.js"></script><script>window.spot.db("x")</script>`)},
		{Path: accessFileName, Data: []byte(`{"allow":["a@example.com"],"note":"window.spot /api/"}`)},
		{Path: "functions/api.js", Data: []byte("export function onRequest() {}")},
		{Path: "_routes.json", Data: []byte("{}")},
		{Path: "_headers", Data: []byte("/*\n  X-Test: yes")},
		{Path: "_redirects", Data: []byte("/old /new 301")},
	}}
	got := checkCloudflareEligibility(snap)
	if got.Eligible {
		t.Fatalf("eligibility = eligible, want rejected")
	}
	for _, want := range []string{
		"functions/",
		"_routes.json",
		"_headers",
		"_redirects",
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

func TestCloudflareEligibilityRejectsTooManyFiles(t *testing.T) {
	got := checkCloudflareEligibility(cloudflareSnapshot{FileCount: maxCloudflareFiles + 1})
	if got.Eligible || len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "20000-file") {
		t.Fatalf("eligibility = %+v, want direct-upload file limit", got)
	}
}

func TestCloudflareAssetHashMatchesWrangler(t *testing.T) {
	// Vectors generated with the exact blake3-wasm@2.1.5 implementation
	// imported by the current Wrangler deploy helpers.
	for _, tc := range []struct {
		path string
		data []byte
		want string
	}{
		{path: "index.html", data: []byte("hello"), want: "a2b82584e50075886b08927390f2f573"},
		{path: "app.js", data: []byte("hello"), want: "46d49df6b69d8c4431d2d5f02ae3e4a9"},
		{path: "empty.txt", data: nil, want: "f9bc91770fa5e997cbd47fba833629fc"},
		{path: "image.bin", data: []byte{0, 1, 2, 255}, want: "2172bd7be4ca4461cd74312a85d2a041"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := cloudflareAssetHash(tc.path, tc.data); got != tc.want {
				t.Fatalf("cloudflareAssetHash(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
	if cloudflareAssetHash("same.html", []byte("same")) == cloudflareAssetHash("same.txt", []byte("same")) {
		t.Fatal("identical bytes with different extensions must have distinct asset keys")
	}
	if cloudflareAssetHash(".env", []byte("same")) != cloudflareAssetHash("env", []byte("same")) ||
		cloudflareAssetHash("dir/.hidden", []byte("same")) != cloudflareAssetHash("hidden", []byte("same")) {
		t.Fatal("Wrangler/Node treats dotfile basenames as extensionless")
	}
	if cloudflareAssetHash(".env.local", []byte("same")) == cloudflareAssetHash("env", []byte("same")) {
		t.Fatal("Wrangler/Node retains suffixes after a dotfile's leading name")
	}
}

func TestCloudflareDeployAndStorageContentHashesMatch(t *testing.T) {
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	files := []deployFile{
		{path: "assets/app.js", data: []byte("console.log('ok')")},
		{path: "index.html", data: []byte("<h1>ok</h1>")},
	}
	for _, file := range files {
		if err := sites.Put(context.Background(), "demo", file.path, contentTypeFor(file.path, file.data), file.data); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{sites: sites}
	snap, err := srv.snapshotCloudflareSite(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if want := cloudflareContentHashForDeploy(files); snap.ContentHash != want {
		t.Fatalf("stored snapshot hash = %q, deploy hash = %q", snap.ContentHash, want)
	}
}

func TestCloudflareSnapshotOmitsPrivateMeshAccessPolicy(t *testing.T) {
	root := t.TempDir()
	sites, err := NewLocalSiteStore(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	files := []deployFile{
		{path: "index.html", data: []byte("<h1>ok</h1>")},
		{path: accessFileName, data: []byte(`{"allow":["alice@example.com"]}`)},
	}
	for _, file := range files {
		if err := sites.Put(context.Background(), "demo", file.path, contentTypeFor(file.path, file.data), file.data); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := (&Server{sites: sites}).snapshotCloudflareSite(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if snap.FileCount != 1 || len(snap.Files) != 1 || snap.Files[0].Path != "index.html" {
		t.Fatalf("Cloudflare snapshot = %+v, want only public site content", snap)
	}
	if snap.ContentHash != cloudflareContentHashForDeploy(files) {
		t.Fatalf("snapshot hash = %q, deploy hash = %q", snap.ContentHash, cloudflareContentHashForDeploy(files))
	}
	changedPolicy := []deployFile{
		{path: "index.html", data: []byte("<h1>ok</h1>")},
		{path: accessFileName, data: []byte(`{"allow":["bob@example.com"]}`)},
	}
	if cloudflareContentHashForDeploy(files) != cloudflareContentHashForDeploy(changedPolicy) {
		t.Fatal("private mesh policy change made the Cloudflare publication stale")
	}
}

type cancelingFailAfterPutSiteStore struct {
	SiteStorage
	cancel  context.CancelFunc
	maxPuts int
	puts    int
}

type failListSiteStore struct {
	SiteStorage
}

func (s *failListSiteStore) List(context.Context, string) ([]string, error) {
	return nil, errors.New("temporary list failure")
}

func (s *cancelingFailAfterPutSiteStore) Put(ctx context.Context, site, filePath, contentType string, data []byte) error {
	s.puts++
	if s.puts > s.maxPuts {
		s.cancel()
		return io.ErrUnexpectedEOF
	}
	return s.SiteStorage.Put(ctx, site, filePath, contentType, data)
}

func TestCanceledPartialRedeployInvalidatesCloudflareContentHash(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	repo := NewCloudflarePublicationStore(db)
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		siteManager:    registry,
		cloudflarePubs: repo,
		cloudflare: &CloudflarePublisher{cfg: cloudflareConfig{
			Status: cloudflareConfigEnabled, BaseDomain: "pages.example.com",
		}},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
		dbLimit:        NewRateLimiter(1000, 1000),
	}
	handler := srv.routes()
	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>original</h1>"},
		{"later.txt", "original"},
	}))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", initial.Code, initial.Body.String())
	}
	owned, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil || len(owned) != 1 || owned[0].ContentHash == "" {
		t.Fatalf("owned after initial deploy = %+v, %v", owned, err)
	}
	publishedHash := owned[0].ContentHash
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ContentHash: publishedHash, Status: "published",
	}); err != nil {
		t.Fatal(err)
	}

	// The first write changes index.html; the second fails, leaving storage in
	// a state that no successful audit hash describes.
	redeployCtx, cancelRedeploy := context.WithCancel(context.Background())
	srv.sites = &cancelingFailAfterPutSiteStore{SiteStorage: sites, cancel: cancelRedeploy, maxPuts: 1}
	failed := httptest.NewRecorder()
	redeploy := deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>partially changed</h1>"},
		{"later.txt", "this write fails"},
	}).WithContext(redeployCtx)
	handler.ServeHTTP(failed, redeploy)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("partial redeploy = %d %s, want 500", failed.Code, failed.Body.String())
	}
	rc, _, err := sites.Open(context.Background(), "demo", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(changed), "partially changed") {
		t.Fatalf("stored index = %q, want proof of partial mutation", changed)
	}

	mine := httptest.NewRecorder()
	handler.ServeHTTP(mine, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if mine.Code != http.StatusOK {
		t.Fatalf("mine = %d %s, want 200", mine.Code, mine.Body.String())
	}
	var body struct {
		Sites []struct {
			Cloudflare cloudflareStatusJSON `json:"cloudflare"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(mine.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].Cloudflare.ContentHash == "" ||
		body.Sites[0].Cloudflare.ContentHash == publishedHash {
		t.Fatalf("mine response = %s, want exact changed hash after partial failed redeploy", mine.Body.String())
	}
}

func TestFailedPartialDeleteMarksContentHashUncertain(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		siteManager:    registry,
		cloudflarePubs: NewCloudflarePublicationStore(db),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	handler := srv.routes()
	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>original</h1>"},
		{"one.txt", "one"},
		{"two.txt", "two"},
	}))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", initial.Code, initial.Body.String())
	}

	srv.sites = &failAfterRemoveSiteStore{SiteStorage: sites, maxRemoves: 1}
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if deleted.Code != http.StatusInternalServerError {
		t.Fatalf("partial delete = %d %s, want 500", deleted.Code, deleted.Body.String())
	}
	remaining, err := sites.List(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 || remaining[0] != accessFileName {
		t.Fatalf("remaining files = %v, want deny-all policy plus two ordinary files after one removal", remaining)
	}
	owned, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || !owned[0].ContentHashUncertain {
		t.Fatalf("owned after failed delete = %+v, want uncertain content hash", owned)
	}
}

func TestMySitesDoesNotFallbackToPublicationHashWhenLegacyDeployIsNewer(t *testing.T) {
	admin := &fakeSiteAdmin{owned: []OwnedSite{{
		SiteRecord: SiteRecord{Name: "demo", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}}
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ContentHash: "older-publication-hash", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin:      admin,
		sites:          &listCountingSiteStore{},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		cloudflarePubs: repo,
		cloudflare: &CloudflarePublisher{cfg: cloudflareConfig{
			Status: cloudflareConfigEnabled, BaseDomain: "pages.example.com",
		}},
	}
	rec := httptest.NewRecorder()
	srv.handleMySites(rec, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mine = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []struct {
			Cloudflare cloudflareStatusJSON `json:"cloudflare"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].Cloudflare.ContentHash != "" {
		t.Fatalf("mine response = %s, want no fallback to older publication", rec.Body.String())
	}
}

func TestMySitesDoesNotFallbackToPublicationHashWhenContentHashUncertain(t *testing.T) {
	admin := &fakeSiteAdmin{owned: []OwnedSite{{
		SiteRecord: SiteRecord{Name: "demo", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		// A failed mutation can overlap a publish that finishes later. The newer
		// publication timestamp does not make its pre-mutation snapshot current.
		ContentHashUncertain: true,
	}}}
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ContentHash: "pre-mutation-publication-hash", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin:      admin,
		sites:          &listCountingSiteStore{},
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		cloudflarePubs: repo,
		cloudflare: &CloudflarePublisher{cfg: cloudflareConfig{
			Status: cloudflareConfigEnabled, BaseDomain: "pages.example.com",
		}},
	}
	rec := httptest.NewRecorder()
	srv.handleMySites(rec, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mine = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []struct {
			Cloudflare cloudflareStatusJSON `json:"cloudflare"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].Cloudflare.ContentHash != "" {
		t.Fatalf("mine response = %s, want no fallback while content is uncertain", rec.Body.String())
	}
}

func TestMySitesRecomputesPublishedHashWhenAuditHashIsUncertain(t *testing.T) {
	admin := &fakeSiteAdmin{owned: []OwnedSite{{
		SiteRecord:           SiteRecord{Name: "demo", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		ContentHashUncertain: true,
	}}}
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(context.Background(), "demo", "index.html", "text/html", []byte("<h1>current</h1>")); err != nil {
		t.Fatal(err)
	}
	snapshotServer := &Server{sites: sites}
	snap, err := snapshotServer.snapshotCloudflareSite(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ContentHash: snap.ContentHash, Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin:      admin,
		sites:          sites,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		cloudflarePubs: repo,
		cloudflare: &CloudflarePublisher{cfg: cloudflareConfig{
			Status: cloudflareConfigEnabled, BaseDomain: "pages.example.com",
		}},
	}
	rec := httptest.NewRecorder()
	srv.handleMySites(rec, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mine = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []struct {
			Cloudflare cloudflareStatusJSON `json:"cloudflare"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].Cloudflare.ContentHash != snap.ContentHash {
		t.Fatalf("mine response = %s, want exact current snapshot hash %q", rec.Body.String(), snap.ContentHash)
	}
}

func TestMySitesMarksHashUncertainWhenSiteWasTouchedAfterLatestAudit(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	actor := Identity{Email: "alice@example.com", Name: "Alice"}
	authz, err := registry.AuthorizeDeploy(context.Background(), "demo", actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(context.Background(), DeployAuditEvent{
		Site: "demo", Actor: actor, Action: "create", Status: "success", ContentHash: "last-audited-hash",
		ContentGeneration: authz.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a process exit after a later deploy was authorized and storage
	// may have changed, but before RecordDeploy could persist its outcome.
	if _, err := registry.AuthorizeDeploy(context.Background(), "demo", actor); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ContentHash: "last-audited-hash", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin:      registry,
		sites:          &listCountingSiteStore{},
		resolver:       NewStaticResolver(actor.Email, actor.Name, nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		cloudflarePubs: repo,
		cloudflare: &CloudflarePublisher{cfg: cloudflareConfig{
			Status: cloudflareConfigEnabled, BaseDomain: "pages.example.com",
		}},
	}
	rec := httptest.NewRecorder()
	srv.handleMySites(rec, sitesRequest(http.MethodGet, "/api/sites/mine"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mine = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []struct {
			Cloudflare cloudflareStatusJSON `json:"cloudflare"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].Cloudflare.ContentHash != "" {
		t.Fatalf("mine response = %s, want unknown hash for unaudited content attempt", rec.Body.String())
	}
}

func TestPreserveAccessFileLimitDoesNotDirtyCurrentHash(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(10000, 10000),
	}
	initial := httptest.NewRecorder()
	srv.routes().ServeHTTP(initial, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>current</h1>"},
		{accessFileName, `{"allow":["alice@example.com"]}`},
	}))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", initial.Code, initial.Body.String())
	}
	before, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].ContentHashUncertain || before[0].ContentHash == "" {
		t.Fatalf("site before rejected deploy = %+v, want current hash", before)
	}

	files := make([][2]string, maxDeployFiles)
	for i := range files {
		files[i] = [2]string{fmt.Sprintf("file-%04d.txt", i), "x"}
	}
	rejected := httptest.NewRecorder()
	srv.routes().ServeHTTP(rejected, deployRequestOrderedFields(t, "spot.localhost", "demo", files,
		map[string]string{"preserve_access": "true"}))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("preserve overflow = %d %s, want 400", rejected.Code, rejected.Body.String())
	}
	after, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ContentHashUncertain || after[0].ContentHash != before[0].ContentHash {
		t.Fatalf("site after rejected deploy = %+v, want prior current hash %q", after, before[0].ContentHash)
	}
}

func TestReadOnlyDeployListFailureDoesNotDirtyCurrentHash(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	initial := httptest.NewRecorder()
	srv.routes().ServeHTTP(initial, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>current</h1>"},
	}))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", initial.Code, initial.Body.String())
	}
	before, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	srv.sites = &failListSiteStore{SiteStorage: sites}
	failed := httptest.NewRecorder()
	srv.routes().ServeHTTP(failed, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>new</h1>"},
	}))
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("list failure deploy = %d %s, want 500", failed.Code, failed.Body.String())
	}
	after, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || after[0].ContentHashUncertain || after[0].ContentHash != before[0].ContentHash {
		t.Fatalf("site after read-only deploy failure = %+v, want prior current hash %q", after, before[0].ContentHash)
	}
}

func TestReadOnlyDeleteListFailureDoesNotDirtyCurrentHash(t *testing.T) {
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		cloudflarePubs: NewCloudflarePublicationStore(db),
		resolver:       NewStaticResolver("alice@example.com", "Alice", nil),
		spotDomain:     "spot.localhost",
		trustedProxies: testTrustedProxies(t),
		deployLimit:    NewRateLimiter(1000, 1000),
	}
	handler := srv.routes()
	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, deployRequestOrdered(t, "spot.localhost", "demo", [][2]string{
		{"index.html", "<h1>current</h1>"},
	}))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial deploy = %d %s, want 200", initial.Code, initial.Body.String())
	}
	before, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	srv.sites = &failListSiteStore{SiteStorage: sites}
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("list failure delete = %d %s, want 500", failed.Code, failed.Body.String())
	}
	after, err := registry.SitesOwnedBy(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || after[0].ContentHashUncertain || after[0].ContentHash != before[0].ContentHash {
		t.Fatalf("site after read-only delete failure = %+v, want prior current hash %q", after, before[0].ContentHash)
	}
}

func TestCloudflareAssetBatchesDeduplicateSharedAssets(t *testing.T) {
	files := []cloudflareSiteFile{
		{Path: "a/icon.svg", Hash: "shared", Data: []byte("same")},
		{Path: "b/icon.svg", Hash: "shared", Data: []byte("same")},
	}
	batches := cloudflareAssetBatches(files, map[string]struct{}{"shared": {}})
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("batches = %+v, want one upload for shared asset key", batches)
	}
}

type fakeCloudflareAPI struct {
	existingProject        bool
	dnsRecords             []cloudflareDNSRecord
	fail                   map[string]error
	calls                  []string
	uploaded               []string
	deletedDNS             []string
	lastDNSZone            string
	lastAccountID          string
	addDomainRecordOnError bool
	domainStatus           string
	accessApp              *cloudflareAccessApplication
	accessSpecs            []cloudflareAccessApplicationSpec
	accessAppOnCreateError bool
	accessChallengeMissing bool
	accessChallengeErr     error
	deployments            []map[string]string
	failDeploymentNumber   int
}

type sequenceSiteManager struct {
	allowed []bool
	calls   int
}

func (m *sequenceSiteManager) CanManageSite(context.Context, string, Identity) (bool, error) {
	index := m.calls
	m.calls++
	if index >= len(m.allowed) {
		return false, nil
	}
	return m.allowed[index], nil
}

func (f *fakeCloudflareAPI) err(op string) error {
	if f.fail == nil {
		return nil
	}
	return f.fail[op]
}

func (f *fakeCloudflareAPI) GetProject(_ context.Context, accountID, _ string) (*cloudflareProject, error) {
	f.calls = append(f.calls, "get-project")
	f.lastAccountID = accountID
	if err := f.err("get-project"); err != nil {
		return nil, err
	}
	if !f.existingProject {
		return nil, errCloudflareNotFound
	}
	return &cloudflareProject{Name: "spot-demo"}, nil
}

func (f *fakeCloudflareAPI) CreateProject(_ context.Context, accountID, _ string) error {
	f.calls = append(f.calls, "create-project")
	f.lastAccountID = accountID
	if err := f.err("create-project"); err != nil {
		return err
	}
	f.existingProject = true
	return nil
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

func (f *fakeCloudflareAPI) CreateDeployment(_ context.Context, _, _ string, manifest map[string]string) (cloudflareDeployment, error) {
	f.calls = append(f.calls, "create-deployment")
	copyManifest := make(map[string]string, len(manifest))
	for path, hash := range manifest {
		copyManifest[path] = hash
	}
	f.deployments = append(f.deployments, copyManifest)
	if f.failDeploymentNumber == len(f.deployments) {
		return cloudflareDeployment{}, errors.New("ambiguous deployment failure")
	}
	return cloudflareDeployment{ID: "dep-1", URL: "https://dep.pages.dev"}, f.err("create-deployment")
}

func (f *fakeCloudflareAPI) AddDomain(_ context.Context, _ string, projectName, hostname string) error {
	f.calls = append(f.calls, "add-domain")
	err := f.err("add-domain")
	if err != nil && f.addDomainRecordOnError {
		f.dnsRecords = append(f.dnsRecords, cloudflareDNSRecord{
			ID: "dns-from-ambiguous-domain", Type: "CNAME", Name: hostname, Content: projectName + ".pages.dev",
		})
	}
	return err
}

func (f *fakeCloudflareAPI) GetDomain(context.Context, string, string, string) (*cloudflareDomain, error) {
	f.calls = append(f.calls, "get-domain")
	if err := f.err("get-domain"); err != nil {
		return nil, err
	}
	status := f.domainStatus
	if status == "" {
		status = "active"
	}
	return &cloudflareDomain{Name: "demo.pages.example.com", Status: status}, nil
}

func (f *fakeCloudflareAPI) DeleteDomain(_ context.Context, accountID, _, _ string) error {
	f.calls = append(f.calls, "delete-domain")
	f.lastAccountID = accountID
	return f.err("delete-domain")
}

func (f *fakeCloudflareAPI) ListDNSRecords(_ context.Context, zoneID, _ string) ([]cloudflareDNSRecord, error) {
	f.calls = append(f.calls, "list-dns")
	f.lastDNSZone = zoneID
	return f.dnsRecords, f.err("list-dns")
}

func (f *fakeCloudflareAPI) CreateDNSRecord(_ context.Context, _ string, hostname, target string) (cloudflareDNSRecord, error) {
	f.calls = append(f.calls, "create-dns")
	if err := f.err("create-dns"); err != nil {
		return cloudflareDNSRecord{}, err
	}
	record := cloudflareDNSRecord{ID: "dns-created", Type: "CNAME", Name: hostname, Content: target, Proxied: true}
	f.dnsRecords = append(f.dnsRecords, record)
	return record, nil
}

func (f *fakeCloudflareAPI) UpdateDNSRecord(_ context.Context, _ string, recordID, hostname, target string, proxied bool) (cloudflareDNSRecord, error) {
	f.calls = append(f.calls, "update-dns")
	if err := f.err("update-dns"); err != nil {
		return cloudflareDNSRecord{}, err
	}
	for i := range f.dnsRecords {
		if f.dnsRecords[i].ID != recordID {
			continue
		}
		f.dnsRecords[i].Type = "CNAME"
		f.dnsRecords[i].Name = hostname
		f.dnsRecords[i].Content = target
		f.dnsRecords[i].Proxied = proxied
		return f.dnsRecords[i], nil
	}
	return cloudflareDNSRecord{}, errCloudflareNotFound
}

func (f *fakeCloudflareAPI) DeleteDNSRecord(_ context.Context, zoneID, recordID string) error {
	f.calls = append(f.calls, "delete-dns")
	f.lastDNSZone = zoneID
	f.deletedDNS = append(f.deletedDNS, recordID)
	return f.err("delete-dns")
}

func (f *fakeCloudflareAPI) DeleteProject(_ context.Context, accountID, _ string) error {
	f.calls = append(f.calls, "delete-project")
	f.lastAccountID = accountID
	if err := f.err("delete-project"); err != nil {
		return err
	}
	f.existingProject = false
	return nil
}

func (f *fakeCloudflareAPI) CreateAccessApplication(_ context.Context, accountID string, spec cloudflareAccessApplicationSpec) (cloudflareAccessApplication, error) {
	f.calls = append(f.calls, "create-access-app")
	f.lastAccountID = accountID
	if err := f.err("create-access-app"); err != nil {
		if f.accessAppOnCreateError {
			f.accessApp = &cloudflareAccessApplication{ID: "access-app-1", Name: spec.Name, Domain: spec.Domain}
			f.accessSpecs = append(f.accessSpecs, spec)
		}
		return cloudflareAccessApplication{}, err
	}
	app := cloudflareAccessApplication{ID: "access-app-1", Name: spec.Name, Domain: spec.Domain}
	f.accessApp = &app
	f.accessSpecs = append(f.accessSpecs, spec)
	return app, nil
}

func (f *fakeCloudflareAPI) FindAccessApplications(_ context.Context, accountID, name string) ([]cloudflareAccessApplication, error) {
	f.calls = append(f.calls, "find-access-apps")
	f.lastAccountID = accountID
	if err := f.err("find-access-apps"); err != nil {
		return nil, err
	}
	if f.accessApp == nil || f.accessApp.Name != name {
		return nil, nil
	}
	return []cloudflareAccessApplication{*f.accessApp}, nil
}

func (f *fakeCloudflareAPI) UpdateAccessApplication(_ context.Context, accountID, appID string, spec cloudflareAccessApplicationSpec) error {
	f.calls = append(f.calls, "update-access-app")
	f.lastAccountID = accountID
	if err := f.err("update-access-app"); err != nil {
		return err
	}
	f.accessApp = &cloudflareAccessApplication{ID: appID, Name: spec.Name, Domain: spec.Domain}
	f.accessSpecs = append(f.accessSpecs, spec)
	return nil
}

func (f *fakeCloudflareAPI) DeleteAccessApplication(_ context.Context, accountID, appID string) error {
	f.calls = append(f.calls, "delete-access-app")
	f.lastAccountID = accountID
	if err := f.err("delete-access-app"); err != nil {
		return err
	}
	if f.accessApp == nil || f.accessApp.ID != appID {
		return errCloudflareNotFound
	}
	f.accessApp = nil
	return nil
}

func (f *fakeCloudflareAPI) AccessChallenge(_ context.Context, _ string) (bool, error) {
	f.calls = append(f.calls, "check-access-challenge")
	if f.accessChallengeErr != nil {
		return false, f.accessChallengeErr
	}
	return !f.accessChallengeMissing, nil
}

type blockingGetProjectCloudflareAPI struct {
	*fakeCloudflareAPI
	started chan struct{}
	release chan struct{}
}

func (f *blockingGetProjectCloudflareAPI) GetProject(ctx context.Context, accountID, projectName string) (*cloudflareProject, error) {
	close(f.started)
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.fakeCloudflareAPI.GetProject(ctx, accountID, projectName)
}

type blockingUploadTokenCloudflareAPI struct {
	*fakeCloudflareAPI
	started chan struct{}
	release chan struct{}
}

func (f *blockingUploadTokenCloudflareAPI) GetUploadToken(ctx context.Context, accountID, projectName string) (string, error) {
	close(f.started)
	select {
	case <-f.release:
		return f.fakeCloudflareAPI.GetUploadToken(ctx, accountID, projectName)
	case <-ctx.Done():
		return "", ctx.Err()
	}
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
		AccessIDPID:   "otp-idp",
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

func cloudflarePublishRequestWithBody(body string) *http.Request {
	req := sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func cloudflareResolveAccessHTTPRequest(confirmAbsent bool) *http.Request {
	body := fmt.Sprintf(`{"confirm_absent":%t}`, confirmAbsent)
	req := sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/access/resolve")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func cloudflareResolveProjectHTTPRequest(resolution string) *http.Request {
	body := fmt.Sprintf(`{"resolution":%q}`, resolution)
	req := sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/project/resolve")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func cloudflareResolveLegacyHTTPRequest(confirm bool) *http.Request {
	body := fmt.Sprintf(`{"confirm_resources_removed":%t}`, confirm)
	req := sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/legacy/resolve")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestCloudflareRestrictedPublishProtectsEveryPublicHostname(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{
		"visibility":"restricted",
		"emails":["ZED@example.com","amy@example.com","amy@example.com"]
	}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("restricted publish = %d %s, want 200", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.AccessMode != cloudflareAccessRestricted || !pub.AccessManaged || pub.AccessAppID != "access-app-1" {
		t.Fatalf("restricted publication = %+v", pub)
	}
	wantEmails := []string{"amy@example.com", "zed@example.com"}
	if strings.Join(pub.AccessEmails, ",") != strings.Join(wantEmails, ",") {
		t.Fatalf("access emails = %v, want %v", pub.AccessEmails, wantEmails)
	}
	if len(cf.accessSpecs) != 2 {
		t.Fatalf("Access specs = %+v, want initial create then post-activation update", cf.accessSpecs)
	}
	if got := strings.Join(cf.accessSpecs[0].Destinations, ","); got != "spot-demo.pages.dev,*.spot-demo.pages.dev" {
		t.Fatalf("initial Access destinations = %q, want pages.dev protected before placeholder deployment", got)
	}
	if got := strings.Join(cf.accessSpecs[1].Destinations, ","); got != "demo.pages.example.com,spot-demo.pages.dev,*.spot-demo.pages.dev" {
		t.Fatalf("final Access destinations = %q", got)
	}
	if cf.accessSpecs[1].IdentityID != "otp-idp" {
		t.Fatalf("Access identity provider = %q", cf.accessSpecs[1].IdentityID)
	}
	if len(cf.deployments) != 2 || cf.deployments[0]["/index.html"] == cf.deployments[1]["/index.html"] {
		t.Fatalf("deployments = %+v, want generated placeholder before the real site", cf.deployments)
	}
	createAccess, createDeployment := -1, -1
	addDomain, protectCustom, firstChallenge, realDeployment := -1, -1, -1, -1
	deploymentCount := 0
	for i, call := range cf.calls {
		if call == "create-access-app" {
			createAccess = i
		}
		if call == "create-deployment" {
			deploymentCount++
			if createDeployment < 0 {
				createDeployment = i
			} else {
				realDeployment = i
			}
		}
		if call == "update-access-app" {
			protectCustom = i
		}
		if call == "check-access-challenge" && firstChallenge < 0 {
			firstChallenge = i
		}
		if call == "add-domain" {
			addDomain = i
		}
	}
	if createAccess < 0 || createDeployment < 0 || createAccess > createDeployment {
		t.Fatalf("calls = %v, want Access before deployment", cf.calls)
	}
	if deploymentCount != 2 || addDomain < 0 || protectCustom < 0 || firstChallenge < 0 || realDeployment < 0 ||
		addDomain > protectCustom || protectCustom > firstChallenge || firstChallenge > realDeployment ||
		countCall(cf.calls, "check-access-challenge") != 3 {
		t.Fatalf("calls = %v, want domain attach, custom-domain Access, three edge checks, then the real deployment", cf.calls)
	}
}

func TestCloudflareRestrictedPublishKeepsPlaceholderUntilAccessReachesEdge(t *testing.T) {
	cf := &fakeCloudflareAPI{accessChallengeMissing: true}
	srv, repo := newCloudflareTestServer(t, cf)
	srv.cloudflare.accessActivationTimeout = 10 * time.Millisecond
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Email access protection") {
		t.Fatalf("restricted publish = %d %s, want edge-protection timeout", rec.Code, rec.Body.String())
	}
	placeholder := cloudflarePendingDeploymentFiles()[0]
	if len(cf.deployments) != 1 || cf.deployments[0]["/index.html"] != placeholder.Hash {
		t.Fatalf("deployments = %+v, want only the non-sensitive placeholder", cf.deployments)
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.DNSManaged || pub.DNSRecordID != "" {
		t.Fatalf("publication after edge timeout = %+v, want rolled-back retry state", pub)
	}
	if !slicesContain(cf.calls, "delete-domain") || !slicesContain(cf.calls, "delete-dns") {
		t.Fatalf("calls = %v, want exposed custom hostname rolled back", cf.calls)
	}
}

func TestCloudflarePublicPublishWaitsForDomainActivation(t *testing.T) {
	cf := &fakeCloudflareAPI{domainStatus: "pending"}
	srv, repo := newCloudflareTestServer(t, cf)
	srv.cloudflare.domainActivationTimeout = 10 * time.Millisecond
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Publishing timed out") {
		t.Fatalf("public publish = %d %s, want domain activation timeout", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "activating-domain" || pub.DeploymentID != "dep-1" || !pub.DNSManaged {
		t.Fatalf("publication after activation timeout = %+v, want resumable committed deployment", pub)
	}
	if countCall(cf.calls, "get-domain") == 0 {
		t.Fatalf("calls = %v, want domain activation check", cf.calls)
	}
}

func TestCloudflarePublicToRestrictedWaitsForAccessBeforeDeploying(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s, want 200", public.Code, public.Body.String())
	}
	if len(cf.deployments) != 1 {
		t.Fatalf("public deployments = %v, want one", cf.deployments)
	}

	cf.calls = nil
	cf.accessChallengeMissing = true
	srv.cloudflare.accessActivationTimeout = 10 * time.Millisecond
	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusBadGateway || !strings.Contains(restricted.Body.String(), "Email access protection") {
		t.Fatalf("restricted transition = %d %s, want edge-protection timeout", restricted.Code, restricted.Body.String())
	}
	if len(cf.deployments) != 1 || countCall(cf.calls, "create-deployment") != 0 {
		t.Fatalf("deployments = %v, calls = %v; want no new snapshot before Access reaches the edge", cf.deployments, cf.calls)
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "protecting-custom-domain" || !pub.AccessManaged || pub.AccessMode != cloudflareAccessRestricted {
		t.Fatalf("publication after edge timeout = %+v, want protected retry marker", pub)
	}

	cf.calls = nil
	cf.accessChallengeMissing = false
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusOK {
		t.Fatalf("restricted retry = %d %s, want 200", retry.Code, retry.Body.String())
	}
	firstChallenge, deployment := -1, -1
	for i, call := range cf.calls {
		if call == "check-access-challenge" && firstChallenge < 0 {
			firstChallenge = i
		}
		if call == "create-deployment" {
			deployment = i
		}
	}
	if len(cf.deployments) != 2 || firstChallenge < 0 || deployment < 0 || firstChallenge > deployment {
		t.Fatalf("deployments = %v, calls = %v; want edge challenge before restricted snapshot", cf.deployments, cf.calls)
	}
}

func TestCloudflarePublishIgnoresPrivateMeshAccessPolicy(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	internalPolicy := []byte(`{"allow":["alice@example.com"],"download":false}`)
	for _, name := range []string{accessFileName, "_ACCESS.JSON"} {
		if err := srv.sites.Put(context.Background(), "demo", name, "application/json", internalPolicy); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(
		`{"visibility":"restricted","emails":["external@example.net"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish site with internal policy = %d %s, want 200", rec.Code, rec.Body.String())
	}
	for _, uploaded := range cf.uploaded {
		if isSiteAccessFile(uploaded) {
			t.Fatalf("uploaded files = %v, private mesh policy must not leave Spot", cf.uploaded)
		}
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || strings.Join(pub.AccessEmails, ",") != "external@example.net" {
		t.Fatalf("Cloudflare access state = %+v, want independent external allowlist", pub)
	}
}

func TestCloudflarePublishValidatesVisibilityBeforeSideEffects(t *testing.T) {
	for _, body := range []string{
		`{"visibility":"restricted","emails":[]}`,
		`{"visibility":"public","emails":["a@example.com"]}`,
		`{"visibility":"restricted","emails":["not an email"]}`,
		`{"visibility":"friends","emails":[]}`,
		`{"visibility":"public","unexpected":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			cf := &fakeCloudflareAPI{}
			srv, _ := newCloudflareTestServer(t, cf)
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid publish = %d %s, want 400", rec.Code, rec.Body.String())
			}
			if len(cf.calls) != 0 {
				t.Fatalf("Cloudflare calls = %v, want none", cf.calls)
			}
		})
	}
}

func TestCloudflareRestrictedPublishRequiresAccessConfiguration(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, _ := newCloudflareTestServer(t, cf)
	srv.cloudflare.cfg.AccessIDPID = ""
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["a@example.com"]}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("restricted publish without Access = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if len(cf.calls) != 0 {
		t.Fatalf("Cloudflare calls = %v, want none", cf.calls)
	}
}

func TestCloudflarePublicationCanTransitionRestrictedAndPublic(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public","emails":[]}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}
	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusOK {
		t.Fatalf("restrict publish = %d %s", restricted.Code, restricted.Body.String())
	}
	open := httptest.NewRecorder()
	srv.routes().ServeHTTP(open, cloudflarePublishRequestWithBody(`{"visibility":"public","emails":[]}`))
	if open.Code != http.StatusOK {
		t.Fatalf("make public = %d %s", open.Code, open.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.AccessMode != cloudflareAccessPublic || pub.AccessManaged || pub.AccessAppID != "" || len(pub.AccessEmails) != 0 {
		t.Fatalf("public publication after transition = %+v", pub)
	}
	if cf.accessApp != nil {
		t.Fatalf("Access app after public transition = %+v", cf.accessApp)
	}
}

func TestCloudflarePublicToRestrictedKeepsDurableStateHonestUntilAccessExists(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}

	cf.fail = map[string]error{"create-access-app": errors.New("ambiguous create failure")}
	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusBadGateway {
		t.Fatalf("restricted transition = %d %s, want 502", restricted.Code, restricted.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "restricting" || pub.AccessMode != cloudflareAccessPublic || pub.AccessManaged || pub.AccessAppID != "" {
		t.Fatalf("durable state after uncertain Access create = %+v, want restricting but still truthfully public", pub)
	}
	if pub.RequestedAccessMode != cloudflareAccessRestricted || strings.Join(pub.RequestedAccessEmails, ",") != "friend@example.com" {
		t.Fatalf("pending policy after uncertain Access create = %+v, want restricted friend@example.com", pub)
	}
}

func TestCloudflareUncertainAccessResolutionClearsConfirmedAbsentAttempt(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-access-app": errors.New("connection reset")}}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("uncertain restricted publish = %d %s, want 502", first.Code, first.Body.String())
	}

	cf.fail = nil
	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveAccessHTTPRequest(true))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve absent Access create = %d %s, want 200", resolved.Code, resolved.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.AccessManaged || pub.AccessAppID != "" ||
		pub.RequestedAccessMode != cloudflareAccessRestricted || strings.Join(pub.RequestedAccessEmails, ",") != "friend@example.com" {
		t.Fatalf("publication after manual Access resolution = %+v", pub)
	}

	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish after manual Access resolution = %d %s, want 200", unpublish.Code, unpublish.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUncertainAccessResolutionAdoptsLateApplication(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-access-app": errors.New("response lost")}}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("uncertain restricted publish = %d %s, want 502", first.Code, first.Body.String())
	}
	cf.fail = nil
	cf.accessApp = &cloudflareAccessApplication{ID: "late-app", Name: "Spot: demo", Domain: "spot-demo.pages.dev"}

	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveAccessHTTPRequest(true))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve late Access app = %d %s, want 200", resolved.Code, resolved.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || !pub.AccessManaged || pub.AccessAppID != "late-app" || pub.Status != "restricting" {
		t.Fatalf("adopted late Access application = %+v", pub)
	}
}

func TestCloudflarePublicRetryReconcilesUncertainAccessCreateBeforeContinuing(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}

	cf.fail = map[string]error{"create-access-app": errors.New("response lost")}
	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusBadGateway {
		t.Fatalf("restricted transition = %d %s, want 502", restricted.Code, restricted.Body.String())
	}

	// A public retry while Access is still invisible must preserve the marker.
	unresolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(unresolved, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if unresolved.Code != http.StatusBadGateway {
		t.Fatalf("unresolved public retry = %d %s, want 502", unresolved.Code, unresolved.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "restricting" || pub.AccessManaged || pub.AccessAppID != "" {
		t.Fatalf("publication after unresolved public retry = %+v", pub)
	}

	cf.fail = nil
	cf.accessApp = &cloudflareAccessApplication{
		ID: "late-app", Name: "Spot: demo", Domain: "demo.pages.example.com",
	}
	reconciled := httptest.NewRecorder()
	srv.routes().ServeHTTP(reconciled, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if reconciled.Code != http.StatusOK {
		t.Fatalf("reconciled public retry = %d %s", reconciled.Code, reconciled.Body.String())
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.AccessMode != cloudflareAccessPublic || pub.AccessManaged || pub.AccessAppID != "" || cf.accessApp != nil {
		t.Fatalf("public publication after reconciliation = %+v, Access app = %+v", pub, cf.accessApp)
	}
	if countCall(cf.calls, "create-access-app") != 1 {
		t.Fatalf("calls = %v, public retry must not repeat Access creation", cf.calls)
	}
}

func TestCloudflareRestrictedPublishReconcilesAmbiguousAccessCreate(t *testing.T) {
	cf := &fakeCloudflareAPI{
		fail:                   map[string]error{"create-access-app": errors.New("response lost")},
		accessAppOnCreateError: true,
	}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("restricted publish with ambiguous create = %d %s", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.AccessAppID != "access-app-1" || !pub.AccessManaged || pub.AccessMode != cloudflareAccessRestricted {
		t.Fatalf("reconciled publication = %+v", pub)
	}
	if countCall(cf.calls, "create-access-app") != 1 {
		t.Fatalf("calls = %v, want exactly one Access create", cf.calls)
	}
}

func TestCloudflareDefinitiveAccessCreateRejectionCanRetry(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{
		"create-access-app": &cloudflareAPIError{statusCode: http.StatusBadRequest, message: "invalid identity provider"},
	}}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("rejected Access create = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.AccessManaged || pub.AccessAppID != "" {
		t.Fatalf("publication after definitive Access rejection = %+v, want retryable failure", pub)
	}

	cf.fail = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after definitive rejection = %d %s", retry.Code, retry.Body.String())
	}
	if countCall(cf.calls, "create-access-app") != 2 {
		t.Fatalf("calls = %v, want rejected create and retry", cf.calls)
	}
}

func TestCloudflareAccessCreateConflictDoesNotAdoptConcurrentApp(t *testing.T) {
	cf := &fakeCloudflareAPI{
		fail:                   map[string]error{"create-access-app": errCloudflareConflict},
		accessAppOnCreateError: true,
	}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("conflicting Access create = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.AccessManaged || pub.AccessAppID != "" {
		t.Fatalf("publication after Access conflict = %+v, must not claim concurrent app", pub)
	}
	if cf.accessApp == nil || slicesContain(cf.calls, "update-access-app") || slicesContain(cf.calls, "delete-access-app") {
		t.Fatalf("calls = %v, concurrent Access app = %+v, want untouched external app", cf.calls, cf.accessApp)
	}
}

func TestCloudflareAccessStateWriteFailureClearsMarkerAfterRollback(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_access_app_id_write
		BEFORE UPDATE ON site_cloudflare_publications
		WHEN OLD.status = 'restricting' AND NEW.access_app_id <> ''
		BEGIN SELECT RAISE(ABORT, 'simulated Access state write failure'); END`); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("Access state failure = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.AccessManaged || pub.AccessAppID != "" || cf.accessApp != nil {
		t.Fatalf("publication after rolled-back Access state write = %+v, app = %+v", pub, cf.accessApp)
	}
	if _, err := repo.db.ExecContext(context.Background(), `DROP TRIGGER fail_access_app_id_write`); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after rolled-back Access state write = %d %s", retry.Code, retry.Body.String())
	}
	if countCall(cf.calls, "create-access-app") != 2 {
		t.Fatalf("calls = %v, want cleaned first app and fresh retry", cf.calls)
	}
}

func TestCloudflareRestrictedPublishRecoversInterruptedAccessCreate(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	pub.Status = "restricting"
	if err := repo.Upsert(context.Background(), *pub); err != nil {
		t.Fatal(err)
	}
	cf.accessApp = &cloudflareAccessApplication{
		ID: "recovered-app", Name: "Spot: demo", Domain: "demo.pages.example.com",
	}
	cf.calls = nil

	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusOK {
		t.Fatalf("retry interrupted restriction = %d %s", restricted.Code, restricted.Body.String())
	}
	if countCall(cf.calls, "create-access-app") != 0 {
		t.Fatalf("calls = %v, retry must adopt the exact existing app instead of creating another", cf.calls)
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.AccessAppID != "recovered-app" || !pub.AccessManaged {
		t.Fatalf("recovered publication = %+v", pub)
	}
}

func TestCloudflareRestrictedPublishNeverRepeatsUncertainAccessCreate(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	pub.Status = "restricting"
	if err := repo.Upsert(context.Background(), *pub); err != nil {
		t.Fatal(err)
	}
	cf.calls = nil

	restricted := httptest.NewRecorder()
	srv.routes().ServeHTTP(restricted, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if restricted.Code != http.StatusBadGateway {
		t.Fatalf("retry unresolved restriction = %d %s, want 502", restricted.Code, restricted.Body.String())
	}
	if countCall(cf.calls, "create-access-app") != 0 {
		t.Fatalf("calls = %v, uncertain retry must never issue a second create", cf.calls)
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "restricting" || pub.AccessMode != cloudflareAccessPublic || pub.LastError == "" {
		t.Fatalf("unresolved publication = %+v", pub)
	}
}

func TestCloudflareUnpublishRetainsUncertainAccessCreateUntilItCanReconcile(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	public := httptest.NewRecorder()
	srv.routes().ServeHTTP(public, cloudflarePublishRequestWithBody(`{"visibility":"public"}`))
	if public.Code != http.StatusOK {
		t.Fatalf("public publish = %d %s", public.Code, public.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	pub.Status = "restricting"
	if err := repo.Upsert(context.Background(), *pub); err != nil {
		t.Fatal(err)
	}
	cf.calls = nil

	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("unpublish unresolved Access create = %d %s, want 502", first.Code, first.Body.String())
	}
	if slicesContain(cf.calls, "delete-project") {
		t.Fatalf("calls = %v, unresolved cleanup must retain Pages and durable state", cf.calls)
	}
	if still, err := repo.Get(context.Background(), "demo"); err != nil || still == nil {
		t.Fatalf("publication after unresolved cleanup = %+v, %v", still, err)
	}

	cf.accessApp = &cloudflareAccessApplication{
		ID: "late-app", Name: "Spot: demo", Domain: "demo.pages.example.com",
	}
	second := httptest.NewRecorder()
	srv.routes().ServeHTTP(second, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if second.Code != http.StatusOK {
		t.Fatalf("unpublish reconciled Access create = %d %s", second.Code, second.Body.String())
	}
	if cf.accessApp != nil {
		t.Fatalf("late Access app after cleanup = %+v", cf.accessApp)
	}
	if remaining, err := repo.Get(context.Background(), "demo"); err != nil || remaining != nil {
		t.Fatalf("publication after reconciled cleanup = %+v, %v", remaining, err)
	}
}

func TestCloudflareFirstRestrictedUnpublishReconcilesPagesDomainAccessApp(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-access-app": errors.New("response lost")}}
	srv, repo := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if publish.Code != http.StatusBadGateway {
		t.Fatalf("first restricted publish = %d %s, want 502", publish.Code, publish.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "restricting" || pub.DeploymentID != "" {
		t.Fatalf("uncertain first publication = %+v", pub)
	}

	cf.fail = nil
	cf.accessApp = &cloudflareAccessApplication{
		ID: "late-pages-app", Name: "Spot: demo", Domain: "spot-demo.pages.dev",
	}
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish reconciled first Access app = %d %s", unpublish.Code, unpublish.Body.String())
	}
	if cf.accessApp != nil {
		t.Fatalf("Pages-domain Access app after cleanup = %+v", cf.accessApp)
	}
	if remaining, err := repo.Get(context.Background(), "demo"); err != nil || remaining != nil {
		t.Fatalf("publication after cleanup = %+v, %v", remaining, err)
	}
}

func TestCloudflareRestrictedRetryKeepsProtectionAfterAmbiguousRealDeployment(t *testing.T) {
	cf := &fakeCloudflareAPI{failDeploymentNumber: 2}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("ambiguous real deployment = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "deploying-restricted" || !pub.AccessManaged || pub.DeploymentID != "" {
		t.Fatalf("publication after ambiguous real deployment = %+v", pub)
	}
	if cf.accessApp == nil || cf.accessApp.Domain != "demo.pages.example.com" {
		t.Fatalf("Access app after ambiguous real deployment = %+v, want custom hostname protected", cf.accessApp)
	}

	cf.failDeploymentNumber = 0
	cf.fail = map[string]error{"get-project": errors.New("temporary project lookup failure")}
	transient := httptest.NewRecorder()
	srv.routes().ServeHTTP(transient, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if transient.Code != http.StatusBadGateway {
		t.Fatalf("transient protected retry = %d %s, want 502", transient.Code, transient.Body.String())
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "deploying-restricted" || !pub.AccessManaged {
		t.Fatalf("publication after transient retry = %+v, want protected recovery marker", pub)
	}
	if cf.accessApp == nil || cf.accessApp.Domain != "demo.pages.example.com" {
		t.Fatalf("Access app after transient retry = %+v, want custom hostname protected", cf.accessApp)
	}

	cf.fail = nil
	cf.calls = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry protected deployment = %d %s", retry.Code, retry.Body.String())
	}
	if !slicesContain(cf.calls, "add-domain") || slicesContain(cf.calls, "create-access-app") {
		t.Fatalf("retry calls = %v, domain attachment must be reasserted and existing Access app reused", cf.calls)
	}
	if countCall(cf.calls, "create-deployment") != 1 {
		t.Fatalf("retry calls = %v, want only the real deployment (no new placeholder)", cf.calls)
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "published" || pub.AccessMode != cloudflareAccessRestricted {
		t.Fatalf("publication after protected retry = %+v", pub)
	}
}

func TestCloudflareDomainActivationStopsAtTerminalStatusAndDeadline(t *testing.T) {
	t.Run("terminal status", func(t *testing.T) {
		publisher := &CloudflarePublisher{client: &fakeCloudflareAPI{domainStatus: "blocked"}}
		err := publisher.waitForDomainActive(context.Background(), "acct", "project", "site.example.com")
		if err == nil || !strings.Contains(err.Error(), `terminal status "blocked"`) {
			t.Fatalf("wait error = %v, want blocked terminal status", err)
		}
	})

	t.Run("internal deadline", func(t *testing.T) {
		publisher := &CloudflarePublisher{
			client:                  &fakeCloudflareAPI{domainStatus: "pending"},
			domainActivationTimeout: 10 * time.Millisecond,
		}
		started := time.Now()
		err := publisher.waitForDomainActive(context.Background(), "acct", "project", "site.example.com")
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait error = %v, want internal deadline", err)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("internal deadline took %s", time.Since(started))
		}
	})
}

func countCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func TestCloudflareUnpublishKeepsAccessUntilPagesIsRemoved(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, _ := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if publish.Code != http.StatusOK {
		t.Fatalf("restricted publish = %d %s", publish.Code, publish.Body.String())
	}
	cf.calls = nil
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s", unpublish.Code, unpublish.Body.String())
	}
	projectDelete, accessDelete := -1, -1
	for i, call := range cf.calls {
		if call == "delete-project" {
			projectDelete = i
		}
		if call == "delete-access-app" {
			accessDelete = i
		}
	}
	if projectDelete < 0 || accessDelete < 0 || accessDelete < projectDelete {
		t.Fatalf("cleanup calls = %v, want Access removed after Pages", cf.calls)
	}
}

func TestCloudflareUnpublishNeverDeletesReplacementAfterProjectRemoval(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if publish.Code != http.StatusOK {
		t.Fatalf("restricted publish = %d %s", publish.Code, publish.Body.String())
	}

	cf.fail = map[string]error{"delete-access-app": errors.New("temporary Access cleanup failure")}
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("partial unpublish = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.ProjectManaged || pub.Status != "project-deleted" || !pub.AccessManaged {
		t.Fatalf("publication after project removal = %+v, want durable deleted-project marker", pub)
	}

	// Another actor may now claim the deterministic project name. The cleanup
	// retry must remove only Spot's remaining Access state, never that project.
	cf.existingProject = true
	cf.fail = nil
	cf.calls = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if retry.Code != http.StatusOK {
		t.Fatalf("cleanup retry = %d %s, want 200", retry.Code, retry.Body.String())
	}
	if slicesContain(cf.calls, "delete-project") || slicesContain(cf.calls, "delete-domain") {
		t.Fatalf("cleanup calls = %v, replacement project and domain must remain untouched", cf.calls)
	}
	if !cf.existingProject {
		t.Fatal("cleanup retry deleted the replacement project")
	}
}

func TestCloudflareUnpublishRequiresResolutionAfterAmbiguousProjectDelete(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if publish.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", publish.Code, publish.Body.String())
	}

	cf.fail = map[string]error{"delete-project": errors.New("project delete response lost")}
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("ambiguous project delete = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.ProjectManaged || pub.Status != "deleting-project" {
		t.Fatalf("publication after ambiguous delete = %+v, want manual resolution marker", pub)
	}

	cf.fail = nil
	cf.calls = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if retry.Code != http.StatusBadGateway || !strings.Contains(retry.Body.String(), "ownership is uncertain") {
		t.Fatalf("unresolved cleanup retry = %d %s, want ownership uncertainty", retry.Code, retry.Body.String())
	}
	if slicesContain(cf.calls, "delete-project") {
		t.Fatalf("retry calls = %v, must not repeat an ambiguous project delete", cf.calls)
	}

	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveProjectHTTPRequest("owned"))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve project ownership = %d %s", resolved.Code, resolved.Body.String())
	}
	finish := httptest.NewRecorder()
	srv.routes().ServeHTTP(finish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if finish.Code != http.StatusOK {
		t.Fatalf("resolved cleanup = %d %s", finish.Code, finish.Body.String())
	}
}

func TestCloudflareRestrictedPublishRollsBackUnprotectedCustomDomain(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"update-access-app": errors.New("Access update failed")}}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("restricted publish = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || !pub.AccessManaged || pub.AccessMode != cloudflareAccessRestricted {
		t.Fatalf("failed restricted publication = %+v", pub)
	}
	if pub.DNSManaged || pub.DNSRecordID != "" {
		t.Fatalf("DNS state after rollback = %+v", pub)
	}
	if !slicesContain(cf.calls, "delete-domain") || !slicesContain(cf.calls, "delete-dns") {
		t.Fatalf("calls = %v, want custom domain and DNS rollback", cf.calls)
	}
	if len(cf.accessSpecs) != 1 || len(cf.accessSpecs[0].Destinations) != 2 {
		t.Fatalf("remaining Access spec = %+v, want pages.dev protected while custom-domain setup rolls back", cf.accessSpecs)
	}

	cf.fail = nil
	cf.calls = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusOK {
		t.Fatalf("restricted publish retry = %d %s", retry.Code, retry.Body.String())
	}
	accessIndex, domainIndex := -1, -1
	for i, call := range cf.calls {
		if call == "update-access-app" && accessIndex == -1 {
			accessIndex = i
		}
		if call == "add-domain" && domainIndex == -1 {
			domainIndex = i
		}
	}
	if accessIndex == -1 || domainIndex == -1 || accessIndex > domainIndex {
		t.Fatalf("retry calls = %v, want Access re-established before custom domain attachment", cf.calls)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCloudflareReservationBlocksConcurrentSiteDelete(t *testing.T) {
	cf := &blockingGetProjectCloudflareAPI{
		fakeCloudflareAPI: &fakeCloudflareAPI{},
		started:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	srv, repo := newCloudflareTestServer(t, cf)
	handler := srv.routes()
	publishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
		publishDone <- rec
	}()
	<-cf.started

	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "reserving" {
		t.Fatalf("publication during project lookup = %+v, want durable reservation", pub)
	}
	deleteRec := httptest.NewRecorder()
	handler.ServeHTTP(deleteRec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if deleteRec.Code != http.StatusConflict {
		t.Fatalf("delete during publish = %d %s, want 409", deleteRec.Code, deleteRec.Body.String())
	}

	close(cf.release)
	publishRec := <-publishDone
	if publishRec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s, want 200", publishRec.Code, publishRec.Body.String())
	}
}

func TestCloudflarePublishAndUnpublishAreSerialized(t *testing.T) {
	cf := &blockingGetProjectCloudflareAPI{
		fakeCloudflareAPI: &fakeCloudflareAPI{},
		started:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	srv, repo := newCloudflareTestServer(t, cf)
	handler := srv.routes()
	publishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
		publishDone <- rec
	}()
	<-cf.started

	unpublishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
		unpublishDone <- rec
	}()
	select {
	case rec := <-unpublishDone:
		t.Fatalf("unpublish completed before publish lock released: %d %s", rec.Code, rec.Body.String())
	case <-time.After(50 * time.Millisecond):
	}

	close(cf.release)
	if rec := <-publishDone; rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	if rec := <-unpublishDone; rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s", rec.Code, rec.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after serialized cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflarePublishSurvivesRequestCancellation(t *testing.T) {
	cf := &blockingUploadTokenCloudflareAPI{
		fakeCloudflareAPI: &fakeCloudflareAPI{},
		started:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	srv, repo := newCloudflareTestServer(t, cf)
	handler := srv.routes()
	ctx, cancel := context.WithCancel(context.Background())
	req := sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish").WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		done <- rec
	}()
	<-cf.started
	inFlight, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if inFlight == nil || inFlight.Status != "publishing" {
		t.Fatalf("publication during upload = %+v, want durable publishing phase", inFlight)
	}
	cancel()
	select {
	case rec := <-done:
		t.Fatalf("publish stopped after browser cancellation: %d %s", rec.Code, rec.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	if !srv.cloudflareJobActive("demo") {
		t.Fatal("publication is not marked active after browser cancellation")
	}
	statusRec := httptest.NewRecorder()
	handler.ServeHTTP(statusRec, sitesRequest(http.MethodGet, "/api/sites/demo/cloudflare"))
	var status cloudflareStatusJSON
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status during detached publish = %d %s", statusRec.Code, statusRec.Body.String())
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.OperationActive || status.Publication == nil || status.Publication.Status != "publishing" {
		t.Fatalf("status during detached publish = %+v", status)
	}

	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "already in progress") {
		t.Fatalf("duplicate publish = %d %s, want active-operation conflict", duplicate.Code, duplicate.Body.String())
	}

	close(cf.release)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("detached publish = %d %s, want 200", rec.Code, rec.Body.String())
	}

	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "published" || pub.LastError != "" {
		t.Fatalf("publication after canceled request = %+v, want successful publication", pub)
	}
	if srv.cloudflareJobActive("demo") {
		t.Fatal("publication remains marked active after completion")
	}
}

func TestCloudflareStatusRedactsProviderErrorDetails(t *testing.T) {
	srv, repo := newCloudflareTestServer(t, &fakeCloudflareAPI{})
	rawError := `Get "https://api.cloudflare.com/client/v4/accounts/secret-account/pages/projects/spot-demo": context canceled`
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		Status: "failed", LastError: rawError,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "api.cloudflare.com") || strings.Contains(body, "secret-account") || strings.Contains(body, "/client/v4/") {
		t.Fatalf("status exposes provider details: %s", body)
	}
	if !strings.Contains(body, "Publishing was interrupted; retry to continue") {
		t.Fatalf("status = %s, want safe recovery message", body)
	}
	stored, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.LastError != rawError {
		t.Fatalf("stored diagnostic = %+v, want raw internal error preserved", stored)
	}
}

func TestCloudflareProjectLookupFailureKeepsSafeReservation(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"get-project": errors.New("temporary lookup failure")}}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("publish = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "reserving" || !strings.Contains(pub.LastError, "temporary lookup failure") {
		t.Fatalf("publication = %+v, want failed safe reservation", pub)
	}

	// No external resource was claimed, so recovery may clear this row even
	// when runtime credentials are no longer present.
	srv.cloudflare.cfg.Status = cloudflareConfigDisabled
	srv.cloudflare.client = nil
	cleanup := httptest.NewRecorder()
	srv.routes().ServeHTTP(cleanup, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if cleanup.Code != http.StatusOK {
		t.Fatalf("clear reservation = %d %s, want 200", cleanup.Code, cleanup.Body.String())
	}
}

func TestCloudflareCreateConflictCleanupFailureDoesNotClaimProject(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-project": errCloudflareConflict}}
	srv, repo := newCloudflareTestServer(t, cf)
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_conflict_reservation_delete
		BEFORE DELETE ON site_cloudflare_publications
		BEGIN SELECT RAISE(ABORT, 'simulated cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("publish = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "reserving" || !strings.Contains(pub.LastError, "not managed by Spot") {
		t.Fatalf("publication after conflict cleanup failure = %+v, want non-owning reservation", pub)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err == nil {
		t.Fatal("unpublish unexpectedly cleared the row while DELETE was blocked")
	}
	for _, call := range cf.calls {
		if call == "delete-project" || call == "delete-domain" || call == "delete-dns" {
			t.Fatalf("calls = %v, must not clean up resources for a conflicting unmanaged project", cf.calls)
		}
	}
}

func TestCloudflareCreateConflictStateWriteFailureDoesNotClaimProject(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-project": errCloudflareConflict}}
	srv, repo := newCloudflareTestServer(t, cf)
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_conflict_state_reset
		BEFORE UPDATE ON site_cloudflare_publications
		WHEN OLD.status = 'creating' AND NEW.status = 'reserving'
		BEGIN SELECT RAISE(ABORT, 'simulated state reset failure'); END`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("publish = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.ProjectManaged || pub.Status != "creating" {
		t.Fatalf("publication after failed conflict state write = %+v, want durable non-ownership", pub)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); !errors.Is(err, errCloudflareProjectUncertain) {
		t.Fatalf("unpublish uncertain non-owning state = %v, want project uncertainty", err)
	}
	cf.existingProject = true
	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveProjectHTTPRequest("unmanaged"))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve conflicting project = %d %s, want 200", resolved.Code, resolved.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after unmanaged resolution = %+v, %v", pub, err)
	}
	for _, call := range cf.calls {
		if call == "delete-project" || call == "delete-domain" || call == "delete-dns" {
			t.Fatalf("calls = %v, must not clean up an unmanaged conflicting project", cf.calls)
		}
	}
}

func TestCloudflareRetryConflictDoesNotClaimProject(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-project": errors.New("ambiguous create failure")}}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first publish = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "creating" || pub.ProjectManaged || !strings.Contains(pub.LastError, "ambiguous create failure") {
		t.Fatalf("publication after ambiguous create = %+v, want durable uncertainty marker", pub)
	}
	cf.fail["create-project"] = errCloudflareConflict
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_retry_conflict_state_reset
		BEFORE UPDATE ON site_cloudflare_publications
		WHEN OLD.status = 'creating' AND NEW.status = 'reserving'
		BEGIN SELECT RAISE(ABORT, 'simulated retry state reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if retry.Code != http.StatusBadGateway {
		t.Fatalf("retry = %d %s, want 502", retry.Code, retry.Body.String())
	}
	pub, err = repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.ProjectManaged {
		t.Fatalf("publication after retry conflict = %+v, want durable non-ownership", pub)
	}
	if countCall(cf.calls, "create-project") != 1 {
		t.Fatalf("calls = %v, retry must not repeat an ambiguous project create", cf.calls)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); !errors.Is(err, errCloudflareProjectUncertain) {
		t.Fatalf("unpublish uncertain retry state = %v, want project uncertainty", err)
	}
	cf.existingProject = true
	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveProjectHTTPRequest("unmanaged"))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve retry conflict = %d %s, want 200", resolved.Code, resolved.Body.String())
	}
	for _, call := range cf.calls {
		if call == "delete-project" {
			t.Fatalf("calls = %v, must not delete retry's conflicting project", cf.calls)
		}
	}
}

func TestCloudflareProjectOwnershipWriteFailureResumesFromSuccessMarker(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_project_ownership_write
		BEFORE UPDATE ON site_cloudflare_publications
		WHEN OLD.status = 'claiming-project' AND NEW.project_managed = 1
		BEGIN SELECT RAISE(ABORT, 'simulated project ownership write failure'); END`); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("project ownership write failure = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "claiming-project" || pub.ProjectManaged {
		t.Fatalf("publication after ownership write failure = %+v, want durable success marker", pub)
	}
	if _, err := repo.db.ExecContext(context.Background(), `DROP TRIGGER fail_project_ownership_write`); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry from project success marker = %d %s, want 200", retry.Code, retry.Body.String())
	}
	if countCall(cf.calls, "create-project") != 1 {
		t.Fatalf("calls = %v, want no duplicate project create", cf.calls)
	}
}

func TestCloudflareProjectResolutionReturnsStoredHostname(t *testing.T) {
	cf := &fakeCloudflareAPI{existingProject: true}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", AccountID: "acct", ZoneID: "zone", ProjectName: "spot-demo",
		Hostname: "demo.old.example.com", Status: "creating",
	}); err != nil {
		t.Fatal(err)
	}
	srv.cloudflare.cfg.BaseDomain = "new.example.com"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflareResolveProjectHTTPRequest("owned"))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve project = %d %s", rec.Code, rec.Body.String())
	}
	var body cloudflareStatusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Hostname != "demo.old.example.com" || body.Publication == nil || body.Publication.Hostname != body.Hostname {
		t.Fatalf("resolution response = %+v, want stored publication hostname", body)
	}
}

func TestCloudflareProjectResolutionRejectsInvalidChoice(t *testing.T) {
	cf := &fakeCloudflareAPI{existingProject: true}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", AccountID: "acct", ZoneID: "zone", ProjectName: "spot-demo",
		Hostname: "demo.pages.example.com", Status: "creating",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflareResolveProjectHTTPRequest("guess"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "owned, unmanaged, or absent") {
		t.Fatalf("invalid project resolution = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestCloudflareLegacyCleanupCanBeManuallyResolved(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		Status: "published", CleanupUnknown: true,
	}); err != nil {
		t.Fatal(err)
	}
	blocked := httptest.NewRecorder()
	srv.routes().ServeHTTP(blocked, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("legacy unpublish = %d %s, want 409", blocked.Code, blocked.Body.String())
	}
	resolved := httptest.NewRecorder()
	srv.routes().ServeHTTP(resolved, cloudflareResolveLegacyHTTPRequest(true))
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve legacy cleanup = %d %s, want 200", resolved.Code, resolved.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after legacy resolution = %+v, %v", pub, err)
	}
}

func TestCloudflareLegacyResolutionReportsRepositoryFailure(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		Status: "published", CleanupUnknown: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(context.Background(), `CREATE TRIGGER fail_legacy_resolution
		BEFORE DELETE ON site_cloudflare_publications WHEN OLD.site = 'demo'
		BEGIN SELECT RAISE(ABORT, 'simulated repository failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, cloudflareResolveLegacyHTTPRequest(true))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "simulated repository failure") {
		t.Fatalf("legacy repository failure = %d %s, want redacted 500", rec.Code, rec.Body.String())
	}
}

func TestCloudflareRecreationConflictRetainsOldDNSAndAccessCleanup(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if publish.Code != http.StatusOK {
		t.Fatalf("initial restricted publish = %d %s", publish.Code, publish.Body.String())
	}
	original, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if original == nil || !original.ProjectManaged || !original.DNSManaged || !original.AccessManaged {
		t.Fatalf("initial publication = %+v, want owned project, DNS, and Access", original)
	}

	// Simulate the Pages project being removed and its name being claimed by a
	// different project before Spot can recreate it.
	cf.existingProject = false
	cf.fail = map[string]error{"create-project": errCloudflareConflict}
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, cloudflarePublishRequestWithBody(`{"visibility":"restricted","emails":["friend@example.com"]}`))
	if retry.Code != http.StatusBadGateway {
		t.Fatalf("recreation conflict = %d %s, want 502", retry.Code, retry.Body.String())
	}
	retained, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if retained == nil || retained.ProjectManaged || !retained.DNSManaged || retained.DNSRecordID != original.DNSRecordID ||
		!retained.AccessManaged || retained.AccessAppID != original.AccessAppID {
		t.Fatalf("publication after recreation conflict = %+v, want old DNS/Access ownership retained", retained)
	}

	cf.fail = nil
	cf.calls = nil
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("cleanup after recreation conflict = %d %s", unpublish.Code, unpublish.Body.String())
	}
	if slicesContain(cf.calls, "delete-project") || slicesContain(cf.calls, "delete-domain") {
		t.Fatalf("cleanup calls = %v, replacement project must remain untouched", cf.calls)
	}
	if !slicesContain(cf.calls, "delete-dns") || !slicesContain(cf.calls, "delete-access-app") {
		t.Fatalf("cleanup calls = %v, want old DNS and Access removed", cf.calls)
	}
	if remaining, err := repo.Get(context.Background(), "demo"); err != nil || remaining != nil {
		t.Fatalf("publication after cleanup = %+v, %v", remaining, err)
	}
}

func TestCloudflareUnpublishCleansOwnedResourcesWhenClaimedProjectVanished(t *testing.T) {
	cf := &fakeCloudflareAPI{
		dnsRecords: []cloudflareDNSRecord{{
			ID: "dns-old", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
		}},
		accessApp: &cloudflareAccessApplication{ID: "access-app-1", Name: "Spot: demo", Domain: "demo.pages.example.com"},
	}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", AccountID: "acct", ZoneID: "zone", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		Status: "claiming-project", ProjectManaged: false, DNSManaged: true, DNSRecordID: "dns-old",
		AccessMode: cloudflareAccessRestricted, AccessManaged: true, AccessAppID: "access-app-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if slicesContain(cf.calls, "delete-project") || slicesContain(cf.calls, "delete-domain") {
		t.Fatalf("calls = %v, absent project must not be deleted", cf.calls)
	}
	if !slicesContain(cf.calls, "delete-dns") || !slicesContain(cf.calls, "delete-access-app") {
		t.Fatalf("calls = %v, want retained DNS and Access ownership cleaned", cf.calls)
	}
	if remaining, err := repo.Get(context.Background(), "demo"); err != nil || remaining != nil {
		t.Fatalf("publication after cleanup = %+v, %v", remaining, err)
	}
}

func TestCloudflareRetryRecreatesProjectAfterDefiniteCreateRejection(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"create-project": &cloudflareAPIError{
		statusCode: http.StatusBadRequest,
		message:    "project request rejected",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first publish = %d %s, want 502", first.Code, first.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" {
		t.Fatalf("publication = %+v, want failed creating state", pub)
	}

	cf.fail = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry publish = %d %s, want 200", retry.Code, retry.Body.String())
	}
	var creates int
	for _, call := range cf.calls {
		if call == "create-project" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("create-project calls = %d, want initial attempt and retry", creates)
	}
}

func TestCloudflareFailedUpdatePreservesLastSuccessfulDeployment(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if first.Code != http.StatusOK {
		t.Fatalf("first publish = %d %s", first.Code, first.Body.String())
	}
	original, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.sites.Put(context.Background(), "demo", "index.html", "text/html", []byte("<h1>changed</h1>")); err != nil {
		t.Fatal(err)
	}
	cf.fail = map[string]error{"upload-token": errors.New("temporary upload failure")}
	update := httptest.NewRecorder()
	srv.routes().ServeHTTP(update, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if update.Code != http.StatusBadGateway {
		t.Fatalf("failed update = %d %s, want 502", update.Code, update.Body.String())
	}
	failed, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.DeploymentID != original.DeploymentID || failed.ContentHash != original.ContentHash {
		t.Fatalf("failed update = %+v, want preserved successful deployment %+v", failed, original)
	}
}

func TestCloudflarePublishPersistsDeploymentBeforeDNSFailure(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"list-dns": errors.New("temporary DNS lookup failure")}}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("publish = %d %s, want 502", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" || pub.DeploymentID != "dep-1" || pub.DeploymentURL != "https://dep.pages.dev" {
		t.Fatalf("publication after DNS failure = %+v, want committed deployment details", pub)
	}
	snap, err := srv.snapshotCloudflareSite(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub.ContentHash != snap.ContentHash || pub.FileCount != snap.FileCount || pub.TotalBytes != snap.TotalBytes {
		t.Fatalf("publication snapshot = %+v, want hash/count/bytes from %+v", pub, snap)
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

func TestCloudflarePublishVerifiesConflictingDomainBelongsToProject(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{
		"add-domain": errCloudflareConflict,
		"get-domain": errCloudflareNotFound,
	}}
	srv, repo := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "outside project") {
		t.Fatalf("publish conflicting domain = %d %s, want verified conflict", rec.Code, rec.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.Status != "failed" {
		t.Fatalf("publication = %+v, want failed", pub)
	}
}

func TestCloudflarePublishAcceptsDomainAlreadyOnManagedProject(t *testing.T) {
	cf := &fakeCloudflareAPI{fail: map[string]error{"add-domain": errCloudflareConflict}}
	srv, _ := newCloudflareTestServer(t, cf)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish existing managed domain = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestCloudflareUnpublishRemovesDNSDomainProjectAndRow(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "dns-1", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site:           "demo",
		DNSRecordID:    "dns-1",
		DNSManaged:     true,
		ProjectManaged: true,
		ProjectName:    "spot-demo",
		Hostname:       "demo.pages.example.com",
		Status:         "published",
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

func TestCloudflareUnpublishPreservesPreexistingMatchingDNS(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "preexisting", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if publish.Code != http.StatusOK {
		t.Fatalf("publish = %d %s, want 200", publish.Code, publish.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.DNSManaged || pub.DNSRecordID != "preexisting" {
		t.Fatalf("publication DNS ownership = %+v, want preserved preexisting record", pub)
	}
	if cf.dnsRecords[0].Proxied || countCall(cf.calls, "update-dns") != 0 {
		t.Fatalf("preexisting DNS = %+v, calls = %v; want unowned record left unchanged", cf.dnsRecords[0], cf.calls)
	}
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s, want 200", unpublish.Code, unpublish.Body.String())
	}
	if len(cf.deletedDNS) != 0 {
		t.Fatalf("deleted DNS = %v, want preexisting record preserved", cf.deletedDNS)
	}
}

func TestCloudflarePublishProxiesManagedLegacyDNSRecord(t *testing.T) {
	cf := &fakeCloudflareAPI{
		existingProject: true,
		dnsRecords: []cloudflareDNSRecord{{
			ID: "dns-owned", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
		}},
	}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site:           "demo",
		AccountID:      "acct",
		ZoneID:         "zone",
		DNSRecordID:    "dns-owned",
		DNSManaged:     true,
		ProjectManaged: true,
		AccessMode:     cloudflareAccessPublic,
		ProjectName:    "spot-demo",
		Hostname:       "demo.pages.example.com",
		DeploymentID:   "dep-old",
		Status:         "published",
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if len(cf.dnsRecords) != 1 || !cf.dnsRecords[0].Proxied || countCall(cf.calls, "update-dns") != 1 {
		t.Fatalf("managed DNS = %+v, calls = %v; want one proxy update", cf.dnsRecords, cf.calls)
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || !pub.DNSManaged || pub.DNSRecordID != "dns-owned" {
		t.Fatalf("publication DNS ownership = %+v, want managed dns-owned", pub)
	}
}

func TestCloudflarePublishUpdateRetainsManagedDNSOwnership(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish %d = %d %s, want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || !pub.DNSManaged || pub.DNSRecordID != "dns-created" {
		t.Fatalf("publication after update = %+v, want retained managed DNS", pub)
	}
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusOK {
		t.Fatalf("unpublish = %d %s, want 200", unpublish.Code, unpublish.Body.String())
	}
	if len(cf.deletedDNS) != 1 || cf.deletedDNS[0] != "dns-created" {
		t.Fatalf("deleted DNS = %v, want managed record", cf.deletedDNS)
	}
}

func TestCloudflarePublishUpdateReassertsCustomDomainAttachment(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, _ := newCloudflareTestServer(t, cf)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish %d = %d %s, want 200", i+1, rec.Code, rec.Body.String())
		}
	}
	if countCall(cf.calls, "add-domain") != 2 {
		t.Fatalf("calls = %v, every publish must ensure the Pages custom-domain attachment", cf.calls)
	}
}

func TestCloudflareRetryPreservesDNSWithoutExactOwnershipAfterAmbiguousDomainFailure(t *testing.T) {
	cf := &fakeCloudflareAPI{
		fail:                   map[string]error{"add-domain": errors.New("domain request timed out")},
		addDomainRecordOnError: true,
	}
	srv, repo := newCloudflareTestServer(t, cf)
	first := httptest.NewRecorder()
	srv.routes().ServeHTTP(first, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first publish = %d %s, want 502", first.Code, first.Body.String())
	}
	failed, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if failed == nil || !failed.DNSManaged || failed.DNSRecordID != "" {
		t.Fatalf("failed publication = %+v, want durable DNS ownership intent", failed)
	}

	cf.fail = nil
	retry := httptest.NewRecorder()
	srv.routes().ServeHTTP(retry, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", retry.Code, retry.Body.String())
	}
	pub, err := repo.Get(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.DNSManaged || pub.DNSRecordID != "dns-from-ambiguous-domain" {
		t.Fatalf("retried publication = %+v, want matching DNS preserved without deletion ownership", pub)
	}
}

func TestCloudflareUnpublishPreservesMatchingDNSWithoutExactRecordID(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "recovered", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ZoneID: "zone", DNSManaged: true, ProjectManaged: true,
		ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if len(cf.deletedDNS) != 0 {
		t.Fatalf("deleted DNS = %v, want matching record preserved without exact ownership ID", cf.deletedDNS)
	}
}

func TestCloudflareUnpublishUnownedProjectStillCleansManagedDNS(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "dns-owned", Type: "CNAME", Name: "demo.pages.example.com", Content: "spot-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ZoneID: "zone", DNSRecordID: "dns-owned", DNSManaged: true,
		ProjectManaged: false, ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if len(cf.deletedDNS) != 1 || cf.deletedDNS[0] != "dns-owned" {
		t.Fatalf("deleted DNS = %v, want managed CNAME cleanup", cf.deletedDNS)
	}
	for _, call := range cf.calls {
		if call == "delete-domain" || call == "delete-project" {
			t.Fatalf("calls = %v, must not delete resources for an unowned project", cf.calls)
		}
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after DNS-only cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishContinuesWhenManagedDNSNoLongerMatches(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "replacement", Type: "A", Name: "demo.pages.example.com", Content: "192.0.2.10",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ZoneID: "zone", DNSManaged: true, ProjectManaged: true,
		ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if len(cf.deletedDNS) != 0 {
		t.Fatalf("deleted DNS = %v, want replacement preserved", cf.deletedDNS)
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishPreservesRepurposedStoredDNSRecord(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "stored-id", Type: "A", Name: "demo.pages.example.com", Content: "192.0.2.20",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ZoneID: "zone", DNSManaged: true, DNSRecordID: "stored-id",
		ProjectManaged: true, ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if len(cf.deletedDNS) != 0 {
		t.Fatalf("deleted DNS = %v, want repurposed stored record preserved", cf.deletedDNS)
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishUsesPublicationAccountAndZone(t *testing.T) {
	cf := &fakeCloudflareAPI{dnsRecords: []cloudflareDNSRecord{{
		ID: "dns-1", Type: "CNAME", Name: "demo.old.example.com", Content: "old-demo.pages.dev",
	}}}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", AccountID: "old-account", ZoneID: "old-zone",
		DNSRecordID: "dns-1", DNSManaged: true, ProjectManaged: true,
		ProjectName: "old-demo", Hostname: "demo.old.example.com", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv.cloudflare.cfg.AccountID = "new-account"
	srv.cloudflare.cfg.ZoneID = "new-zone"
	if err := srv.cloudflare.unpublish(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if cf.lastAccountID != "old-account" || cf.lastDNSZone != "old-zone" {
		t.Fatalf("cleanup location = account %q zone %q, want stored old-account/old-zone", cf.lastAccountID, cf.lastDNSZone)
	}
}

func TestCloudflareUnpublishClearsReservationWithoutRuntimeConfig(t *testing.T) {
	srv, repo := newCloudflareTestServer(t, &fakeCloudflareAPI{})
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "reserving",
	}); err != nil {
		t.Fatal(err)
	}
	srv.cloudflare.cfg.Status = cloudflareConfigDisabled
	srv.cloudflare.client = nil
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear reservation = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub != nil {
		t.Fatalf("publication after clear = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishPublishedSiteRequiresRuntimeConfig(t *testing.T) {
	srv, repo := newCloudflareTestServer(t, &fakeCloudflareAPI{})
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectManaged: true, ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv.cloudflare.cfg.Status = cloudflareConfigDisabled
	srv.cloudflare.client = nil
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unpublish without config = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub == nil {
		t.Fatalf("publication after rejected cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishUnownedManagedDNSRequiresRuntimeConfig(t *testing.T) {
	srv, repo := newCloudflareTestServer(t, &fakeCloudflareAPI{})
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		ProjectManaged: false, DNSManaged: true, DNSRecordID: "dns-owned", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	srv.cloudflare.cfg.Status = cloudflareConfigDisabled
	srv.cloudflare.client = nil
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unpublish managed DNS without config = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub == nil {
		t.Fatalf("publication after rejected DNS cleanup = %+v, %v", pub, err)
	}
}

func TestCloudflareUnpublishReauthorizesAfterAcquiringOperationLock(t *testing.T) {
	srv, repo := newCloudflareTestServer(t, &fakeCloudflareAPI{})
	manager := &sequenceSiteManager{allowed: []bool{true, false}}
	srv.siteManager = manager
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "reserving",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stale unpublish authorization = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if manager.calls != 2 {
		t.Fatalf("CanManageSite calls = %d, want initial and under-lock recheck", manager.calls)
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub == nil {
		t.Fatalf("publication after rejected stale unpublish = %+v, %v", pub, err)
	}
}

func TestCloudflareLegacyUnknownCleanupBlocksPublishAndUnpublish(t *testing.T) {
	cf := &fakeCloudflareAPI{}
	srv, repo := newCloudflareTestServer(t, cf)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com",
		CleanupUnknown: true, Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	publish := httptest.NewRecorder()
	srv.routes().ServeHTTP(publish, sitesRequest(http.MethodPost, "/api/sites/demo/cloudflare/publish"))
	if publish.Code != http.StatusConflict {
		t.Fatalf("publish with unknown legacy cleanup = %d %s, want 409", publish.Code, publish.Body.String())
	}
	unpublish := httptest.NewRecorder()
	srv.routes().ServeHTTP(unpublish, sitesRequest(http.MethodDelete, "/api/sites/demo/cloudflare"))
	if unpublish.Code != http.StatusConflict {
		t.Fatalf("unpublish with unknown legacy cleanup = %d %s, want 409", unpublish.Code, unpublish.Body.String())
	}
	if len(cf.calls) != 0 {
		t.Fatalf("Cloudflare calls = %v, want none without known location", cf.calls)
	}
	if pub, err := repo.Get(context.Background(), "demo"); err != nil || pub == nil || !pub.CleanupUnknown {
		t.Fatalf("legacy guard after rejected actions = %+v, %v", pub, err)
	}
}

func TestDeleteSiteWithCloudflarePublicationReturnsConflict(t *testing.T) {
	db := openTestDB(t)
	repo := NewCloudflarePublicationStore(db)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
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

func TestCloudflarePublicationSchemaGuardsPreventOrphan(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.Upsert(context.Background(), cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: "reserving",
	}); err != nil {
		t.Fatal(err)
	}
	// The trigger is the upgrade-path guard for databases whose existing table
	// cannot gain the new foreign key in place. Prove it works independently of
	// foreign-key enforcement.
	if _, err := db.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM sites WHERE name = 'demo'`); err == nil {
		t.Fatal("site delete succeeded with a reserved Cloudflare publication")
	} else if !strings.Contains(err.Error(), "site has a Cloudflare publication") {
		t.Fatalf("site delete error = %v, want publication guard", err)
	}
}

func TestCloudflareRecordFailureRequiresReservation(t *testing.T) {
	repo := NewCloudflarePublicationStore(openTestDB(t))
	err := repo.RecordFailure(context.Background(), "missing", cloudflareSnapshot{}, errors.New("upload failed"))
	if err == nil || !strings.Contains(err.Error(), "reservation not found") {
		t.Fatalf("RecordFailure error = %v, want missing reservation", err)
	}
}

func TestCloudflareRecoveryFailurePreservesRecoveryStatuses(t *testing.T) {
	for _, status := range []string{
		"creating", "claiming-project", "restricting",
		"protecting-custom-domain", "deploying-restricted", "deleting-project",
	} {
		t.Run(status, func(t *testing.T) {
			db := openTestDB(t)
			repo := NewCloudflarePublicationStore(db)
			if _, err := db.ExecContext(context.Background(),
				`INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
				t.Fatal(err)
			}
			if err := repo.Upsert(context.Background(), cloudflarePublication{
				Site: "demo", ProjectName: "spot-demo", Hostname: "demo.pages.example.com", Status: status,
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.RecordRecoveryFailure(context.Background(), "demo", cloudflareSnapshot{}, errors.New("transient retry failure")); err != nil {
				t.Fatal(err)
			}
			pub, err := repo.Get(context.Background(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			if pub == nil || pub.Status != status || pub.LastError != "transient retry failure" {
				t.Fatalf("publication = %+v, want status %q with updated error", pub, status)
			}
		})
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

func TestCloudflareClientReadsDomainAndCreatedDNSRecord(t *testing.T) {
	var seen []string
	var dnsPayloads []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /accounts/acct/pages/projects/spot-demo/domains/demo.pages.example.com":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{
				"name": "demo.pages.example.com", "status": "active",
			}})
		case "POST /zones/zone/dns_records":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			dnsPayloads = append(dnsPayloads, payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{
				"id": "dns-id", "type": "CNAME", "name": "demo.pages.example.com", "content": "spot-demo.pages.dev", "proxied": true,
			}})
		case "PATCH /zones/zone/dns_records/dns-id":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			dnsPayloads = append(dnsPayloads, payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{
				"id": "dns-id", "type": "CNAME", "name": "demo.pages.example.com", "content": "spot-demo.pages.dev", "proxied": true,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	client := NewCloudflareClient("runtime-token")
	client.baseURL = ts.URL
	domain, err := client.GetDomain(context.Background(), "acct", "spot-demo", "demo.pages.example.com")
	if err != nil || domain.Name != "demo.pages.example.com" {
		t.Fatalf("domain = %+v, %v", domain, err)
	}
	record, err := client.CreateDNSRecord(context.Background(), "zone", "demo.pages.example.com", "spot-demo.pages.dev")
	if err != nil || record.ID != "dns-id" || record.Type != "CNAME" || !record.Proxied {
		t.Fatalf("DNS record = %+v, %v", record, err)
	}
	updated, err := client.UpdateDNSRecord(context.Background(), "zone", record.ID, record.Name, record.Content, true)
	if err != nil || !updated.Proxied {
		t.Fatalf("updated DNS record = %+v, %v", updated, err)
	}
	want := []string{
		"GET /accounts/acct/pages/projects/spot-demo/domains/demo.pages.example.com",
		"POST /zones/zone/dns_records",
		"PATCH /zones/zone/dns_records/dns-id",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seen requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
	if len(dnsPayloads) != 2 || dnsPayloads[0]["proxied"] != true || dnsPayloads[1]["proxied"] != true {
		t.Fatalf("DNS payloads = %#v, want proxied records", dnsPayloads)
	}
}

func TestCloudflareClientTreatsAlreadyAttachedDomainAsConflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 8000018, "message": "You have already added this custom domain."}},
		})
	}))
	defer ts.Close()
	client := NewCloudflareClient("runtime-token")
	client.baseURL = ts.URL
	if err := client.AddDomain(context.Background(), "acct", "spot-demo", "demo.pages.example.com"); !errors.Is(err, errCloudflareConflict) {
		t.Fatalf("AddDomain error = %v, want conflict", err)
	}
}

func TestCloudflareClientRecognizesOnlyAccessLoginRedirects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/protected":
			http.Redirect(w, r, "https://team.cloudflareaccess.com/cdn-cgi/access/login/app", http.StatusFound)
		case "/relative":
			http.Redirect(w, r, "/cdn-cgi/access/login/app", http.StatusFound)
		case "/other-redirect":
			http.Redirect(w, r, "https://example.com/login", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "origin content")
		}
	}))
	defer ts.Close()
	client := NewCloudflareClient("runtime-token")
	client.client = ts.Client()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{path: "/protected", want: true},
		{path: "/relative", want: true},
		{path: "/other-redirect", want: false},
		{path: "/origin", want: false},
	} {
		got, err := client.AccessChallenge(context.Background(), ts.URL+tc.path)
		if err != nil {
			t.Fatalf("AccessChallenge(%s): %v", tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("AccessChallenge(%s) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestCloudflareClientManagesAccessApplicationWithExactEmails(t *testing.T) {
	var seen []string
	var payloads []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode Access payload: %v", err)
			}
			payloads = append(payloads, payload)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /accounts/acct/access/apps":
			if r.URL.Query().Get("name") != "Spot: demo" || r.URL.Query().Get("exact") != "true" {
				t.Errorf("Access lookup query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{{
				"id": "app-id", "name": "Spot: demo", "domain": "demo.pages.example.com",
			}}})
		case "POST /accounts/acct/access/apps":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "app-id"}})
		case "PUT /accounts/acct/access/apps/app-id", "DELETE /accounts/acct/access/apps/app-id":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	client := NewCloudflareClient("runtime-token")
	client.baseURL = ts.URL
	spec := cloudflareAccessApplicationSpec{
		Name:         "Spot: demo",
		Domain:       "demo.pages.example.com",
		Destinations: []string{"demo.pages.example.com", "spot-demo.pages.dev", "*.spot-demo.pages.dev"},
		Emails:       []string{"a@example.com", "b@example.com"},
		IdentityID:   "otp-idp",
	}
	app, err := client.CreateAccessApplication(context.Background(), "acct", spec)
	if err != nil || app.ID != "app-id" {
		t.Fatalf("create Access app = %+v, %v", app, err)
	}
	found, err := client.FindAccessApplications(context.Background(), "acct", spec.Name)
	if err != nil || len(found) != 1 || found[0].ID != "app-id" || found[0].Domain != spec.Domain {
		t.Fatalf("find Access app = %+v, %v", found, err)
	}
	if err := client.UpdateAccessApplication(context.Background(), "acct", app.ID, spec); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteAccessApplication(context.Background(), "acct", app.ID); err != nil {
		t.Fatal(err)
	}
	wantSeen := []string{
		"POST /accounts/acct/access/apps",
		"GET /accounts/acct/access/apps",
		"PUT /accounts/acct/access/apps/app-id",
		"DELETE /accounts/acct/access/apps/app-id",
	}
	if strings.Join(seen, "\n") != strings.Join(wantSeen, "\n") {
		t.Fatalf("Access requests = %v, want %v", seen, wantSeen)
	}
	if len(payloads) != 2 {
		t.Fatalf("Access payloads = %d, want create and update", len(payloads))
	}
	payload := payloads[0]
	if payload["domain"] != spec.Domain || payload["type"] != "self_hosted" || payload["auto_redirect_to_identity"] != true {
		t.Fatalf("Access payload = %#v", payload)
	}
	allowed, _ := payload["allowed_idps"].([]any)
	if len(allowed) != 1 || allowed[0] != "otp-idp" {
		t.Fatalf("allowed identity providers = %#v", payload["allowed_idps"])
	}
	destinations, _ := payload["destinations"].([]any)
	if len(destinations) != 3 {
		t.Fatalf("Access destinations = %#v", payload["destinations"])
	}
	policies, _ := payload["policies"].([]any)
	policy, _ := policies[0].(map[string]any)
	include, _ := policy["include"].([]any)
	if len(include) != 2 {
		t.Fatalf("Access exact-email rules = %#v", policy["include"])
	}
	if strings.Contains(fmt.Sprint(policy["include"]), "login_method") {
		t.Fatalf("Access policy used login method instead of exact emails: %#v", policy)
	}
}
