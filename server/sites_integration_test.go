//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestSiteRegistryListsAndDeletes exercises the registry's list and
// delete paths against SQLite.
func TestSiteRegistryListsAndDeletes(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, testSQLitePath(t))
	if err != nil {
		t.Fatalf("open registry database: %v", err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)

	alice := Identity{Email: "it-alice@example.com", Name: "Alice", PeerIP: "100.64.9.1"}
	bob := Identity{Email: "it-bob@example.com", Name: "Bob", PeerIP: "100.64.9.2"}
	siteNames := []string{"it-reg-one", "it-reg-two"}
	cleanup := func() {
		for _, site := range siteNames {
			db.ExecContext(ctx, `DELETE FROM site_deploy_audit WHERE site = ?`, site)
			db.ExecContext(ctx, `DELETE FROM sites WHERE name = ?`, site)
		}
	}
	cleanup()
	defer cleanup()

	for _, site := range siteNames {
		if _, err := registry.AuthorizeDeploy(ctx, site, alice); err != nil {
			t.Fatalf("claim %s: %v", site, err)
		}
	}
	// Two successful deploys of the same site: the listing must report
	// the most recent one's size, and ignore failed attempts.
	for _, event := range []DeployAuditEvent{
		{Site: "it-reg-one", Actor: alice, Action: "create", Status: "success", FileCount: 3, TotalBytes: 100},
		{Site: "it-reg-one", Actor: alice, Action: "update", Status: "success", FileCount: 5, TotalBytes: 999},
		{Site: "it-reg-one", Actor: alice, Action: "update", Status: "failed", FileCount: 9, TotalBytes: 9999},
	} {
		if err := registry.RecordDeploy(ctx, event); err != nil {
			t.Fatalf("record deploy: %v", err)
		}
	}

	owned, err := registry.SitesOwnedBy(ctx, alice)
	if err != nil {
		t.Fatalf("owned sites: %v", err)
	}
	stats := map[string][2]int64{}
	for _, site := range owned {
		stats[site.Name] = [2]int64{int64(site.FileCount), site.TotalBytes}
	}
	if stats["it-reg-one"] != [2]int64{5, 999} {
		t.Errorf("it-reg-one stats = %v, want latest success {5 999}", stats["it-reg-one"])
	}
	if stats["it-reg-two"] != [2]int64{0, 0} {
		t.Errorf("it-reg-two stats = %v, want zero without audits", stats["it-reg-two"])
	}

	if owned, err := registry.SitesOwnedBy(ctx, bob); err != nil {
		t.Fatalf("owned sites for bob: %v", err)
	} else {
		for _, site := range owned {
			if strings.HasPrefix(site.Name, "it-reg-") {
				t.Errorf("bob owns %s, want none of alice's sites", site.Name)
			}
		}
	}

	all, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatalf("all sites: %v", err)
	}
	found := 0
	for _, site := range all {
		if strings.HasPrefix(site.Name, "it-reg-") {
			found++
		}
	}
	if found != 2 {
		t.Errorf("all sites contains %d it-reg-* entries, want 2", found)
	}

	if err := registry.DeleteSite(ctx, "it-reg-one", bob, nil); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("delete by non-owner = %v, want ErrDeployForbidden", err)
	}
	if err := registry.DeleteSite(ctx, "it-reg-none", alice, nil); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("delete missing site = %v, want ErrSiteNotFound", err)
	}

	purgeErr := errors.New("purge boom")
	if err := registry.DeleteSite(ctx, "it-reg-one", alice, func(context.Context) error {
		return purgeErr
	}); !errors.Is(err, purgeErr) {
		t.Fatalf("delete with failing purge = %v, want the purge error", err)
	}
	if owned, _ := registry.SitesOwnedBy(ctx, alice); len(ownedNamed(owned, "it-reg-one")) == 0 {
		t.Fatal("site vanished although the purge failed")
	}

	purged := 0
	if err := registry.DeleteSite(ctx, "it-reg-one", alice, func(context.Context) error {
		purged++
		return nil
	}); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purge ran %d times, want 1", purged)
	}
	if owned, _ := registry.SitesOwnedBy(ctx, alice); len(ownedNamed(owned, "it-reg-one")) != 0 {
		t.Fatal("site still owned after delete")
	}
}

func ownedNamed(owned []OwnedSite, name string) []OwnedSite {
	var out []OwnedSite
	for _, site := range owned {
		if site.Name == name {
			out = append(out, site)
		}
	}
	return out
}

// TestSiteDeleteRoundtrip deploys a site through the handler, gives it a
// document and an upload, deletes it through the API, and verifies that
// the files, uploads, documents, and registry row are all gone.
func TestSiteDeleteRoundtrip(t *testing.T) {
	endpoint := os.Getenv("SPOT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	requireTestS3Endpoint(t, endpoint)
	sites, err := NewSiteStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-sites")
	if err != nil {
		t.Fatalf("site store: %v", err)
	}
	files, err := NewFileStore(endpoint, "rustfsadmin", "rustfsadmin", "spot-uploads")
	if err != nil {
		t.Fatalf("file store: %v", err)
	}
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, testSQLitePath(t))
	if err != nil {
		t.Fatalf("open registry database: %v", err)
	}
	defer db.Close()

	registry := NewSiteRegistry(db, nil)
	store := &DocStore{db: db}
	srv := &Server{
		store:      store,
		sites:      sites,
		files:      files,
		deployAuth: registry,
		siteAdmin:  registry,
		resolver:   NewStaticResolver("it-deleter@example.com", "Integration Deleter", nil),
		spotDomain: "spot.localhost",
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	const site = "it-delete"
	cleanup := func() {
		db.ExecContext(ctx, `DELETE FROM site_deploy_audit WHERE site = ?`, site)
		db.ExecContext(ctx, `DELETE FROM sites WHERE name = ?`, site)
		db.ExecContext(ctx, `DELETE FROM documents WHERE scope = ?`, site)
		if paths, err := sites.List(ctx, site); err == nil {
			for _, p := range paths {
				sites.Remove(ctx, site, p)
			}
		}
		files.RemoveSite(ctx, site)
	}
	cleanup()
	defer cleanup()

	req := deployRequest(t, "spot.localhost", site, map[string]string{
		"index.html": "<h1>doomed</h1>",
		"app.css":    "body{}",
	})
	res, err := http.DefaultClient.Do(mustOutbound(t, req, ts.URL+"/api/deploy"))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deploy status %d (is `just up` running?)", res.StatusCode)
	}

	if _, err := store.Create(ctx, site, "notes", "", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	if _, err := files.Put(ctx, site, "pic.txt", "text/plain", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// A stranger must not be able to delete the site.
	srv.resolver = NewStaticResolver("intruder@example.com", "Intruder", nil)
	del, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/sites/"+site, nil)
	if err != nil {
		t.Fatal(err)
	}
	del.Header.Set("X-Forwarded-Host", "spot.localhost")
	res, err = http.DefaultClient.Do(del)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("delete by stranger = %d, want 403", res.StatusCode)
	}

	srv.resolver = NewStaticResolver("it-deleter@example.com", "Integration Deleter", nil)
	del, err = http.NewRequest(http.MethodDelete, ts.URL+"/api/sites/"+site, nil)
	if err != nil {
		t.Fatal(err)
	}
	del.Header.Set("X-Forwarded-Host", "spot.localhost")
	res, err = http.DefaultClient.Do(del)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var body map[string]any
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	json.Unmarshal(raw, &body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete by owner = %d %s, want 200", res.StatusCode, raw)
	}
	if body["files"].(float64) != 2 {
		t.Errorf("delete response = %v, want 2 files", body)
	}

	if paths, err := sites.List(ctx, site); err != nil || len(paths) != 0 {
		t.Errorf("site files after delete = %v (%v), want none", paths, err)
	}
	if docs, err := store.List(ctx, site, "notes", 10, ""); err != nil || len(docs) != 0 {
		t.Errorf("documents after delete = %v (%v), want none", docs, err)
	}
	if uploads := filesList(ctx, files, site); len(uploads) != 0 {
		t.Errorf("uploads after delete = %v, want none", uploads)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sites WHERE name = ?`, site).Scan(&count); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if count != 0 {
		t.Errorf("registry rows after delete = %d, want 0", count)
	}
}

func filesList(ctx context.Context, f *FileStore, site string) []string {
	var keys []string
	for obj := range f.client.ListObjects(ctx, f.bucket, minio.ListObjectsOptions{
		Prefix:    site + "/",
		Recursive: true,
	}) {
		if obj.Err == nil {
			keys = append(keys, obj.Key)
		}
	}
	return keys
}
