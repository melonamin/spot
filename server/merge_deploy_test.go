package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func (e *typesEnv) mustDeployVersion(site string, files, fields map[string]string) string {
	e.t.Helper()
	rec := e.deploy(site, files, fields)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("deploy %s = %d %s", site, rec.Code, rec.Body.String())
	}
	version, _ := decodeJSON[map[string]any](e.t, rec)["version"].(string)
	if version == "" {
		e.t.Fatalf("deploy %s response has no version: %s", site, rec.Body.String())
	}
	return version
}

// listedVersion is the version a listing reports for one site.
func (e *typesEnv) listedVersion(listing, site string) string {
	e.t.Helper()
	rec := e.get("spot.localhost", "/api/sites/"+listing+"?names="+site)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s listing = %d %s", listing, rec.Code, rec.Body.String())
	}
	body := decodeJSON[struct {
		Sites []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"sites"`
	}](e.t, rec)
	if len(body.Sites) != 1 || body.Sites[0].Name != site {
		e.t.Fatalf("%s listing for %s = %+v", listing, site, body.Sites)
	}
	return body.Sites[0].Version
}

func (e *typesEnv) siteFile(site, path string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.root, site, filepath.FromSlash(path)))
	if err != nil {
		e.t.Fatalf("read %s/%s: %v", site, path, err)
	}
	return string(data)
}

type deployAuditRow struct {
	Action, Status, Mode string
	FileCount            int
	TotalBytes           int64
	ContentHash          string
}

func (e *typesEnv) lastAudit(site string) deployAuditRow {
	e.t.Helper()
	var row deployAuditRow
	if err := e.db.QueryRow(`SELECT action, status, mode, file_count, total_bytes, content_hash
		FROM site_deploy_audit WHERE site = ? ORDER BY id DESC LIMIT 1`, site).Scan(
		&row.Action, &row.Status, &row.Mode, &row.FileCount, &row.TotalBytes, &row.ContentHash); err != nil {
		e.t.Fatal(err)
	}
	return row
}

func TestDeployVersionIsReportedEverywhere(t *testing.T) {
	env := newTypesEnv(t)
	events := make(chan Event, 8)
	env.srv.hub.Subscribe("demo", deployEventsCollection, events)
	version := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)

	if ev := <-events; ev.Version != version || ev.ID != version {
		t.Fatalf("deploy event = %+v, want version %s", ev, version)
	}
	for _, listing := range []string{"mine", "manageable", "visible", "public"} {
		if got := env.listedVersion(listing, "demo"); got != version {
			t.Errorf("%s listing version = %q, want %q", listing, got, version)
		}
	}
	if rec := env.get("demo.spot.localhost", "/api/download"); rec.Code != http.StatusOK || rec.Header().Get("X-Spot-Version") != version {
		t.Fatalf("download = %d X-Spot-Version %q, want %q", rec.Code, rec.Header().Get("X-Spot-Version"), version)
	}

	next := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v2</h1>"}, nil)
	if next == version {
		t.Fatalf("redeploy kept version %s", version)
	}
	if got := env.listedVersion("manageable", "demo"); got != next {
		t.Fatalf("version after redeploy = %q, want %q", got, next)
	}
}

func TestDeletedSiteListsNoVersion(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("gone", map[string]string{"index.html": "<h1>x</h1>"}, nil)
	if rec := env.jsonRequest(http.MethodDelete, "/api/sites/gone", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if got := env.listedVersion("manageable", "gone"); got != "" {
		t.Fatalf("deleted site version = %q, want none", got)
	}
}

func TestConditionalDeploy(t *testing.T) {
	env := newTypesEnv(t)
	v1 := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)

	v2 := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v2</h1>"}, map[string]string{"if_version": v1})
	if v2 == v1 {
		t.Fatal("conditional deploy kept the version")
	}

	// A writer that read v1 is refused now that the site is at v2, and the
	// refusal leaves the site as it was.
	before := env.siteTouch("demo")
	rec := env.deploy("demo", map[string]string{"index.html": "<h1>stale</h1>"}, map[string]string{"if_version": v1})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "site_version_mismatch" {
		t.Fatalf("stale deploy = %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON[map[string]any](t, rec)["current_version"]; got != v2 {
		t.Fatalf("current_version = %v, want %s", got, v2)
	}
	if after := env.siteTouch("demo"); after != before {
		t.Fatalf("refused deploy changed the site row: %+v, want %+v", after, before)
	}
	if got := env.siteFile("demo", "index.html"); got != "<h1>v2</h1>" {
		t.Fatalf("index.html after refused deploy = %q", got)
	}

	// A site that does not exist has no version; the claim is released.
	rec = env.deploy("fresh", map[string]string{"index.html": "<h1>x</h1>"}, map[string]string{"if_version": v1})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "site_version_mismatch" {
		t.Fatalf("conditional create = %d %s", rec.Code, rec.Body.String())
	}
	if got, ok := decodeJSON[map[string]any](t, rec)["current_version"]; !ok || got != "" {
		t.Fatalf("current_version of a missing site = %v (present %v), want empty", got, ok)
	}
	var rows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE name = 'fresh'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused conditional create left %d rows (%v)", rows, err)
	}

	if rec := env.deploy("demo", map[string]string{"index.html": "x"}, map[string]string{"if_version": " "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty if_version = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConditionalDeployDoesNotDiscloseTheVersion(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)
	env.as("stranger@example.com")
	rec := env.deploy("demo", map[string]string{"index.html": "x"}, map[string]string{"if_version": "wrong"})
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "current_version") {
		t.Fatalf("unauthorized conditional deploy = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMergeDeployKeepsOtherFiles(t *testing.T) {
	env := newTypesEnv(t)
	policy := `{"allow":["owner@example.com"]}`
	env.mustDeploy("demo", map[string]string{
		"index.html":      "<title>Demo page</title><h1>v1</h1>",
		"a.txt":           "a1",
		"keep/b.txt":      "b1",
		accessFileName:    policy,
		"_spot.json":      `{"title":"Demo","tags":["notes"]}`,
		"_screenshot.png": "png",
	}, nil)
	v1 := env.listedVersion("manageable", "demo")

	rec := env.deploy("demo", map[string]string{"a.txt": "a2", "new/c.txt": "c1"}, map[string]string{"mode": "merge"})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge = %d %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON[map[string]any](t, rec)
	if body["files"] != float64(7) || body["version"] == v1 {
		t.Fatalf("merge response = %v, want 7 files and a new version", body)
	}
	want := []string{"_access.json", "_screenshot.png", "_spot.json", "a.txt", "index.html", "keep/b.txt", "new/c.txt"}
	if got := env.siteFiles("demo"); !slices.Equal(got, want) {
		t.Fatalf("site files = %v, want %v", got, want)
	}
	if env.siteFile("demo", "a.txt") != "a2" || env.siteFile("demo", accessFileName) != policy {
		t.Fatal("merge did not overwrite a.txt or did not keep the policy")
	}
	meta, err := env.registry.SiteMetadata(t.Context(), "demo")
	if err != nil || meta.Title != "Demo" || !slices.Equal(meta.Tags, []string{"notes"}) {
		t.Fatalf("metadata after merge = %+v %v", meta, err)
	}

	// The audit describes the merged site, so listings and the Cloudflare
	// staleness check stay right.
	audit := env.lastAudit("demo")
	snap, err := env.srv.snapshotCloudflareSite(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, path := range want {
		total += int64(len(env.siteFile("demo", path)))
	}
	if audit.Mode != "merge" || audit.Action != "update" || audit.FileCount != 7 || audit.TotalBytes != total || audit.ContentHash != snap.ContentHash {
		t.Fatalf("merge audit = %+v, want merge/update, 7 files, %d bytes, hash %s", audit, total, snap.ContentHash)
	}

	// A merge that ships _spot.json or index.html re-derives the metadata
	// from the merged site.
	env.mustDeploy("demo", map[string]string{"_spot.json": `{"description":"Updated"}`}, map[string]string{"mode": "merge"})
	if meta, _ := env.registry.SiteMetadata(t.Context(), "demo"); meta.Title != "Demo page" || meta.Description != "Updated" {
		t.Fatalf("metadata after a _spot.json merge = %+v", meta)
	}

	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v2</h1>"}, nil)
	if audit := env.lastAudit("demo"); audit.Mode != "replace" {
		t.Fatalf("replace audit mode = %q", audit.Mode)
	}
}

func TestMergeDeployRefusals(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v1</h1>", "docs/a.txt": "a", "file": "f"}, nil)
	env.mustDeploy("gone", map[string]string{"index.html": "<h1>x</h1>"}, nil)
	if rec := env.jsonRequest(http.MethodDelete, "/api/sites/gone", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	merge := map[string]string{"mode": "merge"}

	for _, tt := range []struct {
		name   string
		site   string
		files  map[string]string
		fields map[string]string
		status int
		code   string
	}{
		{"missing site", "fresh", map[string]string{"a.txt": "a"}, merge, http.StatusNotFound, "site_not_found"},
		{"deleted site", "gone", map[string]string{"a.txt": "a"}, merge, http.StatusNotFound, "site_not_found"},
		{"no files", "demo", map[string]string{".DS_Store": "junk"}, merge, http.StatusBadRequest, ""},
		{"file where a directory is", "demo", map[string]string{"docs": "x"}, merge, http.StatusConflict, "merge_path_conflict"},
		{"directory where a file is", "demo", map[string]string{"file/x.txt": "x"}, merge, http.StatusConflict, "merge_path_conflict"},
		{"unknown mode", "demo", map[string]string{"a.txt": "a"}, map[string]string{"mode": "append"}, http.StatusBadRequest, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var before siteTouch
			if tt.site != "fresh" {
				before = env.siteTouch(tt.site)
			}
			rec := env.deploy(tt.site, tt.files, tt.fields)
			if rec.Code != tt.status || (tt.code != "" && errorCode(t, rec) != tt.code) {
				t.Fatalf("merge = %d %s, want %d %s", rec.Code, rec.Body.String(), tt.status, tt.code)
			}
			if tt.site == "fresh" {
				var rows int
				if err := env.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE name = 'fresh'`).Scan(&rows); err != nil || rows != 0 {
					t.Fatalf("refused merge left %d rows (%v)", rows, err)
				}
				return
			}
			if after := env.siteTouch(tt.site); after != before {
				t.Fatalf("refused merge changed the site row: %+v, want %+v", after, before)
			}
		})
	}
	if got := env.siteFiles("demo"); !slices.Equal(got, []string{"docs/a.txt", "file", "index.html"}) {
		t.Fatalf("site files after refusals = %v", got)
	}
	// An untyped merge needs no index.html and strips no folder.
	env.mustDeploy("demo", map[string]string{"docs/b.txt": "b"}, merge)
	if got := env.siteFiles("demo"); !slices.Equal(got, []string{"docs/a.txt", "docs/b.txt", "file", "index.html"}) {
		t.Fatalf("site files after merge = %v", got)
	}
}

func TestMergeDeployLimitsApplyToTheMergedSite(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)
	for i := 1; i < maxDeployFiles; i++ {
		if err := env.srv.sites.Put(t.Context(), "demo", fmt.Sprintf("f/%04d.txt", i), "text/plain", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(env.siteFiles("demo")); n != maxDeployFiles {
		t.Fatalf("seeded %d files, want %d", n, maxDeployFiles)
	}
	merge := map[string]string{"mode": "merge"}
	rec := env.deploy("demo", map[string]string{"one-more.txt": "x"}, merge)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "merge_too_large" {
		t.Fatalf("merge past the file limit = %d %s", rec.Code, rec.Body.String())
	}
	// Overwriting keeps the count, so it is accepted.
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v2</h1>"}, merge)

	big := filepath.Join(env.root, "big", "blob.bin")
	env.mustDeploy("big", map[string]string{"index.html": "<h1>big</h1>"}, nil)
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, maxDeploySize-int64(len("<h1>big</h1>"))-10); err != nil {
		t.Fatal(err)
	}
	rec = env.deploy("big", map[string]string{"more.txt": strings.Repeat("y", 20)}, merge)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "merge_too_large" {
		t.Fatalf("merge past the size limit = %d %s", rec.Code, rec.Body.String())
	}
	env.mustDeploy("big", map[string]string{"more.txt": strings.Repeat("y", 10)}, merge)
	if audit := env.lastAudit("big"); audit.TotalBytes != maxDeploySize {
		t.Fatalf("merged size = %d, want %d", audit.TotalBytes, maxDeploySize)
	}
}

func TestMergeDeployPreserveAccess(t *testing.T) {
	env := newTypesEnv(t)
	policy := `{"allow":["owner@example.com"]}`
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v1</h1>", accessFileName: policy}, nil)
	merge := map[string]string{"mode": "merge"}

	if rec := env.deploy("demo", map[string]string{accessFileName: policy, "a.txt": "a"},
		map[string]string{"mode": "merge", "preserve_access": "require"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("require with a shipped policy = %d %s", rec.Code, rec.Body.String())
	}
	env.mustDeploy("demo", map[string]string{"a.txt": "a"}, map[string]string{"mode": "merge", "preserve_access": "require"})
	if env.siteFile("demo", accessFileName) != policy {
		t.Fatal("require merge lost the policy")
	}
	// preserve_access=false keeps its meaning: the stored policy goes.
	env.mustDeploy("demo", map[string]string{"b.txt": "b"}, map[string]string{"mode": "merge", "preserve_access": "false"})
	if got := env.siteFiles("demo"); !slices.Equal(got, []string{"a.txt", "b.txt", "index.html"}) {
		t.Fatalf("site files after preserve_access=false merge = %v", got)
	}
	if rec := env.deploy("demo", map[string]string{"c.txt": "c"}, map[string]string{"mode": "merge", "preserve_access": "require"}); rec.Code != http.StatusConflict {
		t.Fatalf("require without a stored policy = %d %s", rec.Code, rec.Body.String())
	}
	// A merge may ship a new policy.
	env.mustDeploy("demo", map[string]string{accessFileName: policy}, merge)
	if env.siteFile("demo", accessFileName) != policy {
		t.Fatal("merged policy was not stored")
	}
}

func TestTypedMergeDeploy(t *testing.T) {
	env := typedDeckEnv(t)
	env.mustDeploy("deck", map[string]string{"card.json": `{"title":"Deck"}`, "media/a.png": "a", "_spot.json": `{"title":"Deck"}`},
		map[string]string{"type": "card@1.0.0"})
	env.mustDeploy("plain", map[string]string{"index.html": "<h1>plain</h1>"}, nil)
	typed := map[string]string{"type": "card@1.0.0", "mode": "merge"}

	env.mustDeploy("deck", map[string]string{"media/b.png": "b"}, typed)
	if got := env.siteFiles("deck"); !slices.Equal(got, []string{"_spot.json", "card.json", "media/a.png", "media/b.png"}) {
		t.Fatalf("typed merge files = %v", got)
	}

	for _, tt := range []struct {
		name   string
		site   string
		files  map[string]string
		fields map[string]string
		status int
		code   string
	}{
		{"outside the data paths", "deck", map[string]string{"notes.txt": "x"}, typed, http.StatusBadRequest, "type_data_invalid"},
		{"viewer entry", "deck", map[string]string{"index.html": "x"}, typed, http.StatusBadRequest, "type_data_invalid"},
		{"invalid JSON", "deck", map[string]string{"card.json": "{"}, typed, http.StatusBadRequest, "type_data_invalid"},
		{"without type", "deck", map[string]string{"media/c.png": "c"}, map[string]string{"mode": "merge"}, http.StatusConflict, "type_required"},
		{"with type_target", "deck", map[string]string{"media/c.png": "c"},
			map[string]string{"type": "card@1.0.0", "type_target": "1.1.0", "mode": "merge"}, http.StatusBadRequest, "invalid_type_field"},
		{"into an untyped site", "plain", map[string]string{"card.json": "{}"}, typed, http.StatusConflict, "site_untyped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := env.siteTouch(tt.site)
			rec := env.deploy(tt.site, tt.files, tt.fields)
			if rec.Code != tt.status || errorCode(t, rec) != tt.code {
				t.Fatalf("typed merge = %d %s, want %d %s", rec.Code, rec.Body.String(), tt.status, tt.code)
			}
			if after := env.siteTouch(tt.site); after != before {
				t.Fatalf("refused merge changed the site row: %+v, want %+v", after, before)
			}
		})
	}

	// A merge may change the track; following then moves the pin.
	rec := env.deploy("deck", map[string]string{"card.json": `{"title":"Deck 2"}`},
		map[string]string{"type": "card@1.0.0", "mode": "merge", "type_track": "major"})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge with type_track = %d %s", rec.Code, rec.Body.String())
	}
	if ref := env.siteType("deck"); ref != (siteTypeRef{Name: "card", Version: "1.1.0", Track: "major"}) {
		t.Fatalf("pin after merge with type_track=major = %+v", ref)
	}
	if got := decodeJSON[map[string]any](t, rec)["version"]; got != env.listedVersion("manageable", "deck") {
		t.Fatalf("merge response version %v differs from the listed one after the follow move", got)
	}
	if got := env.siteFiles("deck"); !slices.Equal(got, []string{"_spot.json", "card.json", "media/a.png", "media/b.png"}) {
		t.Fatalf("files after the track merge = %v", got)
	}
}

func TestRepinAndFollowStampTheVersionTheirEventsCarry(t *testing.T) {
	env := typedDeckEnv(t)
	v1 := env.listedVersion("manageable", "deck")
	events := make(chan Event, 8)
	env.srv.hub.Subscribe("deck", deployEventsCollection, events)

	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.1.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("repin = %d %s", rec.Code, rec.Body.String())
	}
	ev := <-events
	if v2 := env.listedVersion("manageable", "deck"); v2 == v1 || ev.Version != v2 {
		t.Fatalf("repin: event version %q, listed %q, before %q", ev.Version, v2, v1)
	}
	// A track-only change publishes no event and keeps the version.
	v2 := env.listedVersion("manageable", "deck")
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"track":"major"}`); rec.Code != http.StatusOK {
		t.Fatalf("track change = %d %s", rec.Code, rec.Body.String())
	}
	if got := env.listedVersion("manageable", "deck"); got != v2 {
		t.Fatalf("track-only repin changed the version: %q, want %q", got, v2)
	}
	// A follow move stamps a new version too.
	env.mustUpload(cardVersion(t, "1.2.0"))
	ev = <-events
	if v3 := env.listedVersion("manageable", "deck"); v3 == v2 || ev.Version != v3 || ev.SiteType != "card@1.2.0" {
		t.Fatalf("follow: event %+v, listed %q, before %q", ev, v3, v2)
	}
	// A stale writer's conditional deploy is refused after the move.
	rec := env.deploy("deck", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0", "if_version": v2})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "site_version_mismatch" {
		t.Fatalf("deploy against the pre-follow version = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeployAuditRecordsTheModeOfRefusedDeploys(t *testing.T) {
	env := newTypesEnv(t)
	env.mustDeploy("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)
	env.as("stranger@example.com")
	if rec := env.deploy("demo", map[string]string{"a.txt": "a"}, map[string]string{"mode": "merge"}); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger merge = %d %s", rec.Code, rec.Body.String())
	}
	if audit := env.lastAudit("demo"); audit.Status != "denied" || audit.Mode != "merge" {
		t.Fatalf("denied audit = %+v", audit)
	}
}

// cancelAfterPutsSiteStore cancels the request once a number of writes have
// succeeded, like a client that disconnects after the last file is stored.
type cancelAfterPutsSiteStore struct {
	SiteStorage
	cancel context.CancelFunc
	after  int
	puts   int
}

func (s *cancelAfterPutsSiteStore) Put(ctx context.Context, site, path, contentType string, data []byte) error {
	if err := s.SiteStorage.Put(ctx, site, path, contentType, data); err != nil {
		return err
	}
	if s.puts++; s.puts == s.after {
		s.cancel()
	}
	return nil
}

// A deploy that fails after changing live files leaves a new version over
// them, so a writer holding the old version is refused, and a writer that
// reads the site again can publish.
func TestFailedDeployAfterAWriteInvalidatesTheVersion(t *testing.T) {
	env := newTypesEnv(t)
	v1 := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v1</h1>", "a.txt": "original"}, nil)

	live := env.srv.sites
	env.srv.sites = &failAfterPutSiteStore{SiteStorage: live, maxPuts: 1}
	rec := env.deploy("demo", map[string]string{"a.txt": "human edit", "b.txt": "b"}, map[string]string{"mode": "merge"})
	env.srv.sites = live
	if rec.Code != http.StatusInternalServerError || env.siteFile("demo", "a.txt") != "human edit" {
		t.Fatalf("partial merge = %d %s, a.txt %q", rec.Code, rec.Body.String(), env.siteFile("demo", "a.txt"))
	}
	v2 := env.listedVersion("manageable", "demo")
	if v2 == v1 {
		t.Fatal("a deploy that changed live files kept the old version")
	}
	if rec := env.get("demo.spot.localhost", "/api/download"); rec.Header().Get("X-Spot-Version") != v2 {
		t.Fatalf("download X-Spot-Version = %q, want %q", rec.Header().Get("X-Spot-Version"), v2)
	}
	rec = env.deploy("demo", map[string]string{"index.html": "<h1>v1</h1>", "a.txt": "original"}, map[string]string{"if_version": v1})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "site_version_mismatch" {
		t.Fatalf("stale publish after a partial deploy = %d %s", rec.Code, rec.Body.String())
	}
	if got := env.siteFile("demo", "a.txt"); got != "human edit" {
		t.Fatalf("a.txt after the refused publish = %q", got)
	}
	env.mustDeploy("demo", map[string]string{"b.txt": "b"}, map[string]string{"mode": "merge", "if_version": v2})
}

func TestCancelledDeployAfterTheLastWriteInvalidatesTheVersion(t *testing.T) {
	env := newTypesEnv(t)
	v1 := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v1</h1>", "a.txt": "original"}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	live := env.srv.sites
	env.srv.sites = &cancelAfterPutsSiteStore{SiteStorage: live, cancel: cancel, after: 2}
	req := deployRequestOrderedFields(t, "spot.localhost", "demo",
		[][2]string{{"a.txt", "human edit"}, {"b.txt", "b"}}, map[string]string{"mode": "merge"})
	rec := env.do(req.WithContext(ctx))
	env.srv.sites = live
	if rec.Code == http.StatusOK || env.siteFile("demo", "b.txt") != "b" {
		t.Fatalf("cancelled merge = %d %s", rec.Code, rec.Body.String())
	}
	if v2 := env.listedVersion("manageable", "demo"); v2 == v1 {
		t.Fatal("a cancelled deploy that changed live files kept the old version")
	}
	rec = env.deploy("demo", map[string]string{"index.html": "<h1>v1</h1>", "a.txt": "original"}, map[string]string{"if_version": v1})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "site_version_mismatch" {
		t.Fatalf("stale publish after a cancelled deploy = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConcurrentConditionalMergesHaveOneWinner(t *testing.T) {
	env := newTypesEnv(t)
	v1 := env.mustDeployVersion("demo", map[string]string{"index.html": "<h1>v1</h1>"}, nil)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := deployRequestOrderedFields(t, "spot.localhost", "demo",
				[][2]string{{fmt.Sprintf("edit-%d.txt", i), "x"}}, map[string]string{"mode": "merge", "if_version": v1})
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusConflict}) {
		t.Fatalf("concurrent conditional merges = %v, want one 200 and one 409", codes)
	}
	if n := len(env.siteFiles("demo")); n != 2 {
		t.Fatalf("site has %d files, want index.html and the winner's edit", n)
	}
}
