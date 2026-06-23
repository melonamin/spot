package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestBackfillGalleryWritesMetadataAndSpotJSONWithoutTouchingOwner(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatalf("claim site: %v", err)
	}
	before, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("sites before = %d, want 1", len(before))
	}

	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", `<html><head><title>Demo App</title><meta name="description" content="A useful demo"></head><body><h1>Demo</h1></body></html>`)
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: sites, spotDomain: "spot.localhost"}

	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{Write: true, WriteSpotJSON: true})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.MetadataUpdated != 1 || result.SpotJSONWritten != 1 {
		t.Fatalf("result = %+v, want one metadata update and one _spot.json write", result)
	}

	after, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("sites after = %d, want 1", len(after))
	}
	site := after[0]
	if site.OwnerEmail != "alice@example.com" || site.OwnerName != "Alice" {
		t.Fatalf("owner after backfill = %q/%q, want Alice", site.OwnerEmail, site.OwnerName)
	}
	if !site.UpdatedAt.Equal(before[0].UpdatedAt) {
		t.Fatalf("updated_at changed from %s to %s", before[0].UpdatedAt, site.UpdatedAt)
	}
	if site.Title != "Demo App" || site.Description != "A useful demo" {
		t.Fatalf("metadata = %q/%q, want extracted title and description", site.Title, site.Description)
	}

	raw, err := readSiteFile(ctx, sites, "demo", siteMetadataFileName)
	if err != nil {
		t.Fatalf("read _spot.json: %v", err)
	}
	var meta SiteMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse written _spot.json: %v", err)
	}
	if meta.Title != "Demo App" || meta.Description != "A useful demo" {
		t.Fatalf("_spot.json metadata = %+v, want extracted metadata", meta)
	}
}

func TestBackfillGalleryScreenshotsOnlyPublicSites(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	for _, site := range []string{"open", "locked"} {
		if _, err := registry.AuthorizeDeploy(ctx, site, owner); err != nil {
			t.Fatalf("claim %s: %v", site, err)
		}
	}
	dir := t.TempDir()
	writeSiteFile(t, dir, "open", "index.html", "<title>Open</title>")
	writeSiteFile(t, dir, "locked", "index.html", "<title>Locked</title>")
	writeSiteFile(t, dir, "locked", accessFileName, `{"allow":["alice@example.com"]}`)
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: sites, spotDomain: "spot.localhost"}
	png := []byte("\x89PNG\r\n\x1a\nfake-png-body")

	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{
		Write:       true,
		Screenshots: true,
		Scheme:      "http",
		captureScreenshot: func(_ context.Context, site, url string) ([]byte, error) {
			if site != "open" {
				t.Fatalf("captured restricted site %s", site)
			}
			if url != "http://open.spot.localhost/" {
				t.Fatalf("screenshot url = %q, want open site URL", url)
			}
			return png, nil
		},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.ScreenshotsWritten != 1 || result.ScreenshotsSkipped != 1 {
		t.Fatalf("result = %+v, want one screenshot and one restricted skip", result)
	}
	got, err := readSiteFile(ctx, sites, "open", "_screenshot.png")
	if err != nil {
		t.Fatalf("read screenshot: %v", err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("screenshot bytes = %q, want generated png", got)
	}
	if exists, err := siteFileExists(ctx, sites, "locked", "_screenshot.png"); err != nil || exists {
		t.Fatalf("restricted screenshot exists=%v err=%v, want no file", exists, err)
	}
}
