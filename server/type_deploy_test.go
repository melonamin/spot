package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

func (e *typesEnv) siteFiles(site string) []string {
	e.t.Helper()
	paths, err := e.srv.sites.List(e.t.Context(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func TestTypedDeployCreatesAnInstance(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	rec := env.deploy("deck", map[string]string{
		"card.json":   `{"title":"Deck"}`,
		"media/a.png": "png",
		".DS_Store":   "junk",
		"_spot.json":  `{"title":"Deck site"}`,
	}, map[string]string{"type": "card@1.0.0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("typed create = %d %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON[map[string]any](t, rec)
	if got := body["type"].(map[string]any); got["name"] != "card" || got["version"] != "1.0.0" || got["track"] != "pinned" || got["title"] != "Card" {
		t.Fatalf("response type = %v", got)
	}
	// Paths are kept exactly: no folder stripping, junk dropped, no index.
	if got := strings.Join(env.siteFiles("deck"), ","); got != "_spot.json,card.json,media/a.png" {
		t.Fatalf("site files = %s", got)
	}
	if ref := env.siteType("deck"); ref != (siteTypeRef{Name: "card", Version: "1.0.0", Track: "pinned"}) {
		t.Fatalf("pin = %+v", ref)
	}
}

func TestTypedDeployMayBeEmpty(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustDeploy("empty", nil, map[string]string{"type": "card@1.0.0"})
	if rec := env.get("empty.spot.localhost", "/"); rec.Code != http.StatusOK {
		t.Fatalf("empty instance viewer = %d", rec.Code)
	}
	rec := env.get("empty.spot.localhost", "/api/download")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Spot-Type") != "card@1.0.0" {
		t.Fatalf("empty download = %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil || len(zr.File) != 0 {
		t.Fatalf("empty download zip: %v, %d files", err, len(zr.File))
	}
}

func TestTypedDeployValidation(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustUpload(cardVersion(t, "1.1.0"))
	env.as(typesAdminEmail)
	env.jsonRequest(http.MethodPost, "/api/types/card/1.1.0/yank", "")
	env.as(typesOwnerEmail)
	typed := map[string]string{"type": "card@1.0.0"}
	for _, tt := range []struct {
		name   string
		files  map[string]string
		fields map[string]string
		status int
		code   string
		path   string
	}{
		{"root index", map[string]string{"index.html": "<h1>x</h1>"}, typed, 400, "type_data_invalid", "index.html"},
		{"viewer dir", map[string]string{"_type/x.js": "x"}, typed, 400, "type_data_invalid", "_type/x.js"},
		{"outside data", map[string]string{"card.json": "{}", "notes.txt": "x"}, typed, 400, "type_data_invalid", "notes.txt"},
		{"prefix is segment bounded", map[string]string{"mediax.png": "x"}, typed, 400, "type_data_invalid", "mediax.png"},
		{"invalid json", map[string]string{"card.json": "{nope"}, typed, 400, "type_data_invalid", "card.json"},
		{"invalid json under prefix", map[string]string{"media/meta.json": "["}, typed, 400, "type_data_invalid", "media/meta.json"},
		{"absolute path", map[string]string{"/card.json": "{}"}, typed, 400, "type_data_invalid", "/card.json"},
		{"bare name", map[string]string{"card.json": "{}"}, map[string]string{"type": "card"}, 400, "invalid_type_field", ""},
		{"range", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@^1.0.0"}, 400, "invalid_type_field", ""},
		{"build metadata", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0+b"}, 400, "invalid_type_field", ""},
		{"empty type", map[string]string{"card.json": "{}"}, map[string]string{"type": ""}, 400, "invalid_type_field", ""},
		{"track without type", map[string]string{"index.html": "x"}, map[string]string{"type_track": "major"}, 400, "invalid_type_field", ""},
		{"bad track", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0", "type_track": "latest"}, 400, "invalid_type_field", ""},
		{"target on create", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0", "type_target": "1.0.0"}, 400, "invalid_type_field", ""},
		{"unknown version", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@9.0.0"}, 400, "type_not_found", ""},
		{"yanked version", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.1.0"}, 409, "type_yanked", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := env.deploy("fresh", tt.files, tt.fields)
			if rec.Code != tt.status || errorCode(t, rec) != tt.code {
				t.Fatalf("deploy = %d %s, want %d %s", rec.Code, rec.Body.String(), tt.status, tt.code)
			}
			if tt.path != "" && decodeJSON[map[string]any](t, rec)["path"] != tt.path {
				t.Fatalf("path = %v, want %s", decodeJSON[map[string]any](t, rec)["path"], tt.path)
			}
		})
	}
	// Every refusal released its claim: the name is still free.
	if _, err := env.srv.types.SiteType(t.Context(), "fresh"); err != ErrSiteNotFound {
		t.Fatalf("refused creates left a site row: %v", err)
	}
}

func TestTypedDeployPinRules(t *testing.T) {
	env := typedDeckEnv(t)
	data := map[string]string{"card.json": `{"title":"Deck"}`}

	rec := env.deploy("deck", data, nil)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "type_required" || decodeJSON[map[string]any](t, rec)["current"] != "card@1.0.0" {
		t.Fatalf("untyped deploy to typed site = %d %s", rec.Code, rec.Body.String())
	}
	// Also refused under the site lock when the bundle looks like a site.
	if rec := env.deploy("deck", map[string]string{"index.html": "<h1>x</h1>"}, nil); rec.Code != http.StatusConflict || errorCode(t, rec) != "type_required" {
		t.Fatalf("untyped site bundle to typed site = %d %s", rec.Code, rec.Body.String())
	}
	if env.get(deckHost, "/card.json").Code != http.StatusOK {
		t.Fatal("refused deploy changed the site's files")
	}
	env.mustUpload(renamedType(t, "other", "1.0.0"))
	if rec := env.deploy("deck", data, map[string]string{"type": "other@1.0.0"}); rec.Code != http.StatusConflict || errorCode(t, rec) != "type_name_mismatch" {
		t.Fatalf("other type = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.deploy("deck", data, map[string]string{"type": "card@1.1.0"}); rec.Code != http.StatusConflict || errorCode(t, rec) != "type_pin_mismatch" {
		t.Fatalf("pinned site with another version = %d %s", rec.Code, rec.Body.String())
	}
	// type_target repins in the same deploy and validates against the target.
	rec = env.deploy("deck", data, map[string]string{"type": "card@1.0.0", "type_target": "1.1.0"})
	if rec.Code != http.StatusOK || env.siteType("deck") != (siteTypeRef{"card", "1.1.0", "pinned"}) {
		t.Fatalf("type_target = %d %s, pin %+v", rec.Code, rec.Body.String(), env.siteType("deck"))
	}
	// The edit prepared against 1.0.0 cannot silently undo the migration.
	rec = env.deploy("deck", data, map[string]string{"type": "card@1.0.0"})
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "type_pin_mismatch" || decodeJSON[map[string]any](t, rec)["current"] != "card@1.1.0" {
		t.Fatalf("stale type after repin = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.deploy("deck", data, map[string]string{"type": "card@1.1.0", "type_target": "7.0.0"}); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "type_not_found" {
		t.Fatalf("unknown target = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTypedDeployTargetValidatesAgainstTheTarget(t *testing.T) {
	env := typedDeckEnv(t)
	v2 := cardVersion(t, "2.0.0")
	v2[typeManifestFile] = strings.Replace(v2[typeManifestFile], `["card.json","media/"]`, `["deck.json"]`, 1)
	env.mustUpload(v2)
	rec := env.deploy("deck", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0", "type_target": "2.0.0"})
	if rec.Code != http.StatusBadRequest || decodeJSON[map[string]any](t, rec)["path"] != "card.json" {
		t.Fatalf("old data with 2.0.0 target = %d %s", rec.Code, rec.Body.String())
	}
	if env.siteType("deck").Version != "1.0.0" {
		t.Fatal("refused migration changed the pin")
	}
	env.mustDeploy("deck", map[string]string{"deck.json": "{}"}, map[string]string{"type": "card@1.0.0", "type_target": "2.0.0"})
	if got := strings.Join(env.siteFiles("deck"), ","); got != "deck.json" {
		t.Fatalf("migrated files = %s", got)
	}
}

func TestUntypedSiteBecomesTypedAndDeleteClearsThePin(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustDeploy("deck", map[string]string{"index.html": "<h1>old</h1>"}, nil)
	env.mustDeploy("deck", map[string]string{"card.json": `{"title":"x"}`}, map[string]string{"type": "card@1.0.0"})
	if got := strings.Join(env.siteFiles("deck"), ","); got != "card.json" {
		t.Fatalf("files after becoming typed = %s", got)
	}
	if rec := env.get(deckHost, "/"); !strings.Contains(rec.Body.String(), "Card 1.0.0") {
		t.Fatalf("typed root = %q", rec.Body.String())
	}
	if rec := env.do(httptestRequest(http.MethodDelete, "spot.localhost", "/api/sites/deck")); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	var name, version, track string
	if err := env.db.QueryRow(`SELECT type_name, type_version, type_track FROM sites WHERE name = 'deck'`).Scan(&name, &version, &track); err != nil {
		t.Fatal(err)
	}
	if name+version+track != "" {
		t.Fatalf("tombstone keeps the pin %s@%s %s", name, version, track)
	}
	// The owner's recreate is untyped unless it names a type.
	env.mustDeploy("deck", map[string]string{"index.html": "<h1>new</h1>"}, nil)
	if env.siteType("deck").typed() {
		t.Fatal("recreate inherited the deleted site's type")
	}
}

func TestFollowerAcceptsSameMajorType(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	data := map[string]string{"card.json": "{}"}
	env.mustDeploy("deck", data, map[string]string{"type": "card@1.0.0", "type_track": "major"})
	env.mustUpload(cardVersion(t, "1.1.0"))
	if env.siteType("deck").Version != "1.1.0" {
		t.Fatalf("follower did not move: %+v", env.siteType("deck"))
	}
	// Written against 1.0.0 before the follow: accepted, the pin stays.
	rec := env.deploy("deck", data, map[string]string{"type": "card@1.0.0"})
	if rec.Code != http.StatusOK || env.siteType("deck") != (siteTypeRef{"card", "1.1.0", "major"}) {
		t.Fatalf("same-major deploy = %d %s, pin %+v", rec.Code, rec.Body.String(), env.siteType("deck"))
	}
	env.mustUpload(cardVersion(t, "2.0.0"))
	if rec := env.deploy("deck", data, map[string]string{"type": "card@2.0.0"}); rec.Code != http.StatusConflict || errorCode(t, rec) != "type_pin_mismatch" {
		t.Fatalf("other-major deploy on follower = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.deploy("deck", data, map[string]string{"type": "card@1.0.0", "type_target": "1.1.0"}); rec.Code != http.StatusConflict {
		t.Fatalf("type_target with a stale type = %d, want 409", rec.Code)
	}
	// The exception needs the major track.
	env.mustDeploy("deck", data, map[string]string{"type": "card@1.1.0", "type_track": "pinned"})
	if rec := env.deploy("deck", data, map[string]string{"type": "card@1.0.0"}); rec.Code != http.StatusConflict {
		t.Fatalf("stale type on a pinned site = %d, want 409", rec.Code)
	}
}

func TestPublishingKeyCannotRepin(t *testing.T) {
	env := typedDeckEnv(t)
	key := DeployPrincipal{PublishingKey: true}
	current := siteTypeRef{"card", "1.0.0", "pinned"}
	for _, req := range []*typedDeployFields{
		{Name: "card", Version: "1.0.0", Target: "1.1.0", HasTarget: true},
		{Name: "card", Version: "1.0.0", Track: "major", HasTrack: true},
	} {
		_, terr, err := env.srv.planSiteType(t.Context(), key, req, current, false)
		if err != nil || terr == nil || terr.code != "type_repin_forbidden" {
			t.Errorf("publishing key %+v = %v %v, want type_repin_forbidden", req, terr, err)
		}
	}
	// Same track, and creating with a track, are allowed.
	if _, terr, _ := env.srv.planSiteType(t.Context(), key, &typedDeployFields{Name: "card", Version: "1.0.0", Track: "pinned", HasTrack: true}, current, false); terr != nil {
		t.Errorf("same-track key deploy = %v", terr)
	}
	if plan, terr, _ := env.srv.planSiteType(t.Context(), key, &typedDeployFields{Name: "card", Version: "1.0.0", Track: "major", HasTrack: true}, siteTypeRef{}, true); terr != nil || plan.pin.Track != "major" {
		t.Errorf("key create with major = %v %+v", terr, plan)
	}
}

func TestTypedDeployEventsCarryTheSiteType(t *testing.T) {
	env := typedDeckEnv(t)
	events := make(chan Event, 8)
	env.srv.hub.Subscribe("deck", deployEventsCollection, events)
	env.mustDeploy("deck", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0"})
	if ev := <-events; ev.SiteType != "card@1.0.0" {
		t.Fatalf("deploy event site_type = %q", ev.SiteType)
	}
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.1.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("repin = %d", rec.Code)
	}
	select {
	case ev := <-events:
		if ev.Type != "deploy" || ev.SiteType != "card@1.1.0" {
			t.Fatalf("repin event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("repin published no deploy event")
	}
}

func TestRepin(t *testing.T) {
	env := typedDeckEnv(t)
	put := func(body string) *httptest.ResponseRecorder {
		return env.jsonRequest(http.MethodPut, "/api/sites/deck/type", body)
	}

	for _, body := range []string{`{}`, `{"version":"1.0"}`, `{"track":"latest"}`, `{"version":"1.0.0","extra":1}`, `nope`} {
		if rec := put(body); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_type_field" {
			t.Errorf("repin %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := put(`{"version":"9.0.0"}`); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "type_not_found" {
		t.Fatalf("unknown version = %d %s", rec.Code, rec.Body.String())
	}
	rec := put(`{"version":"1.1.0"}`)
	body := decodeJSON[repinResponse](t, rec)
	if rec.Code != http.StatusOK || body.Type["version"] != "1.1.0" || body.Previous["version"] != "1.0.0" || body.Type["track"] != "pinned" {
		t.Fatalf("repin = %d %s", rec.Code, rec.Body.String())
	}

	env.as("stranger@example.com")
	if rec := put(`{"version":"1.0.0"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger repin = %d", rec.Code)
	}
	env.as(typesAdminEmail)
	if rec := put(`{"version":"1.0.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin repin = %d %s", rec.Code, rec.Body.String())
	}
	env.as(typesOwnerEmail)
	req := httptestRequest(http.MethodPut, "spot.localhost", "/api/sites/deck/type")
	req.Body = http.NoBody
	req.Header.Set("Authorization", "Bearer spot_pk_x")
	if rec := env.do(req); rec.Code != http.StatusForbidden || errorCode(t, rec) != "type_repin_forbidden" {
		t.Fatalf("publishing-key repin = %d %s", rec.Code, rec.Body.String())
	}

	// Yanked targets are refused; a yanked current version can be frozen.
	env.as(typesAdminEmail)
	env.jsonRequest(http.MethodPost, "/api/types/card/1.1.0/yank", "")
	env.jsonRequest(http.MethodPost, "/api/types/card/1.0.0/yank", "")
	env.as(typesOwnerEmail)
	if rec := put(`{"version":"1.1.0"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != "type_yanked" {
		t.Fatalf("yanked target = %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(`{"track":"major"}`); rec.Code != http.StatusOK {
		t.Fatalf("track change on a yanked version = %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(`{"track":"pinned"}`); rec.Code != http.StatusOK || env.siteType("deck") != (siteTypeRef{"card", "1.0.0", "pinned"}) {
		t.Fatalf("freeze = %d %+v", rec.Code, env.siteType("deck"))
	}

	env.mustDeploy("plain", map[string]string{"index.html": "x"}, nil)
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/plain/type", `{"track":"major"}`); rec.Code != http.StatusConflict || errorCode(t, rec) != "site_untyped" {
		t.Fatalf("untyped repin = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/nosuch/type", `{"track":"major"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing site repin = %d", rec.Code)
	}
}

func TestRepinValidatesRetainedData(t *testing.T) {
	env := typedDeckEnv(t)
	v2 := cardVersion(t, "2.0.0")
	v2[typeManifestFile] = strings.Replace(v2[typeManifestFile], `["card.json","media/"]`, `["deck.json","card.json"]`, 1)
	env.mustUpload(v2)
	env.mustDeploy("deck", map[string]string{"card.json": "{}", "media/a.png": "png"}, map[string]string{"type": "card@1.0.0"})
	rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"2.0.0"}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "type_data_incompatible" || decodeJSON[map[string]any](t, rec)["path"] != "media/a.png" {
		t.Fatalf("incompatible repin = %d %s", rec.Code, rec.Body.String())
	}
	env.mustDeploy("deck", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0"})
	// Retained JSON must parse under the target too.
	if err := env.srv.sites.Put(t.Context(), "deck", "deck.json", "application/json", []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	rec = env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"2.0.0"}`)
	if rec.Code != http.StatusConflict || decodeJSON[map[string]any](t, rec)["path"] != "deck.json" {
		t.Fatalf("broken retained json = %d %s", rec.Code, rec.Body.String())
	}
	env.srv.sites.Remove(t.Context(), "deck", "deck.json")
	if rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"2.0.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("compatible repin across majors = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRepinWaitsForTheSiteLock(t *testing.T) {
	env := typedDeckEnv(t)
	lock := env.srv.siteMutationLock("deck")
	lock.Lock()
	done := make(chan int, 1)
	go func() { done <- env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.1.0"}`).Code }()
	select {
	case code := <-done:
		lock.Unlock()
		t.Fatalf("repin finished (%d) while a deploy held the site lock", code)
	case <-time.After(100 * time.Millisecond):
	}
	if env.siteType("deck").Version != "1.0.0" {
		lock.Unlock()
		t.Fatal("pin changed under a held site lock")
	}
	lock.Unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("repin after unlock = %d", code)
	}
}

func TestExplicitVersionRepinPinsUnlessTrackMajor(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustUpload(cardVersion(t, "1.1.0"))
	env.mustDeploy("deck", map[string]string{"card.json": "{}"}, map[string]string{"type": "card@1.0.0", "type_track": "major"})
	if env.siteType("deck") != (siteTypeRef{"card", "1.1.0", "major"}) {
		t.Fatalf("create with major = %+v, want following 1.1.0", env.siteType("deck"))
	}
	// A backward repin is not undone by following.
	env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.0.0"}`)
	if env.siteType("deck") != (siteTypeRef{"card", "1.0.0", "pinned"}) {
		t.Fatalf("backward repin = %+v", env.siteType("deck"))
	}
	// Explicit version plus track major follows forward at once.
	rec := env.jsonRequest(http.MethodPut, "/api/sites/deck/type", `{"version":"1.0.0","track":"major"}`)
	if rec.Code != http.StatusOK || env.siteType("deck") != (siteTypeRef{"card", "1.1.0", "major"}) {
		t.Fatalf("repin with major = %d %s, pin %+v", rec.Code, rec.Body.String(), env.siteType("deck"))
	}
	if decodeJSON[repinResponse](t, rec).Type["version"] != "1.1.0" {
		t.Fatalf("response does not report the followed version: %s", rec.Body.String())
	}
}

func TestFollowResolution(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	data := map[string]string{"card.json": "{}"}
	env.mustDeploy("follower", data, map[string]string{"type": "card@1.0.0", "type_track": "major"})
	env.mustDeploy("pinned", data, map[string]string{"type": "card@1.0.0"})

	// Prereleases and other majors are never followed.
	env.mustUpload(cardVersion(t, "1.1.0-rc.1"))
	env.mustUpload(cardVersion(t, "2.0.0"))
	if env.siteType("follower").Version != "1.0.0" {
		t.Fatalf("followed a prerelease or another major: %+v", env.siteType("follower"))
	}
	rec := env.upload(cardVersion(t, "1.1.0"))
	followed := decodeJSON[map[string]any](t, rec)["followed"].([]any)
	if len(followed) != 1 || followed[0].(map[string]any)["site"] != "follower" || followed[0].(map[string]any)["to"] != "1.1.0" {
		t.Fatalf("upload followed = %v", followed)
	}
	if env.siteType("follower").Version != "1.1.0" || env.siteType("pinned").Version != "1.0.0" {
		t.Fatalf("after 1.1.0: follower %+v, pinned %+v", env.siteType("follower"), env.siteType("pinned"))
	}

	// A yank never moves sites, not even back.
	env.as(typesAdminEmail)
	env.jsonRequest(http.MethodPost, "/api/types/card/1.1.0/yank", "")
	if env.siteType("follower").Version != "1.1.0" {
		t.Fatal("yank moved a follower")
	}
	// A yanked version is not followed; un-yanking makes it followable.
	env.mustUpload(cardVersion(t, "1.2.0"))
	env.jsonRequest(http.MethodPost, "/api/types/card/1.2.0/yank", "")
	env.mustDeploy("late", data, map[string]string{"type": "card@1.0.0", "type_track": "major"})
	if env.siteType("late").Version != "1.0.0" {
		t.Fatalf("follow chose a yanked version: %+v", env.siteType("late"))
	}
	env.as(typesAdminEmail)
	unyank := decodeJSON[map[string]any](t, env.jsonRequest(http.MethodDelete, "/api/types/card/1.2.0/yank", ""))
	// follower already moved to 1.2.0 on its upload, before the yank.
	if len(unyank["followed"].([]any)) != 1 || env.siteType("late").Version != "1.2.0" || env.siteType("follower").Version != "1.2.0" {
		t.Fatalf("un-yank followed = %v; late %+v", unyank["followed"], env.siteType("late"))
	}

	// An identical re-upload repairs a follow interrupted after the insert.
	if _, err := env.db.Exec(`UPDATE sites SET type_version = '1.0.0' WHERE name = 'late'`); err != nil {
		t.Fatal(err)
	}
	if rec := env.upload(cardVersion(t, "1.2.0")); rec.Code != http.StatusOK || env.siteType("late").Version != "1.2.0" {
		t.Fatalf("identical re-upload = %d, late %+v", rec.Code, env.siteType("late"))
	}

	// Enabling major on a pinned site follows at once.
	env.as(typesOwnerEmail)
	rec = env.jsonRequest(http.MethodPut, "/api/sites/pinned/type", `{"track":"major"}`)
	if rec.Code != http.StatusOK || env.siteType("pinned").Version != "1.2.0" {
		t.Fatalf("enable major = %d %s, pin %+v", rec.Code, rec.Body.String(), env.siteType("pinned"))
	}

	// Pin moves are audited as action type and do not count as deploys.
	var audits int
	env.db.QueryRow(`SELECT COUNT(*) FROM site_deploy_audit WHERE site = 'follower' AND action = 'type' AND status = 'success'`).Scan(&audits)
	if audits != 2 {
		t.Fatalf("follower type audits = %d, want 2", audits)
	}
	mine := decodeJSON[typedSiteList](t, env.get("spot.localhost", "/api/sites/mine")).Sites
	for _, site := range mine {
		if site["name"] == "follower" && site["file_count"] != float64(1) {
			t.Fatalf("follow audit replaced the last deploy summary: %v", site)
		}
	}
}

func TestSiteListingsCarryTheType(t *testing.T) {
	env := typedDeckEnv(t)
	env.mustDeploy("plain", map[string]string{"index.html": "x"}, nil)
	for _, target := range []string{"/api/sites/mine", "/api/sites/manageable", "/api/sites/visible?names=deck,plain", "/api/sites/public"} {
		sites := decodeJSON[typedSiteList](t, env.get("spot.localhost", target)).Sites
		if len(sites) != 2 {
			t.Fatalf("%s = %d sites", target, len(sites))
		}
		for _, site := range sites {
			typ, ok := site["type"].(map[string]any)
			switch site["name"] {
			case "deck":
				if !ok || typ["name"] != "card" || typ["version"] != "1.0.0" || typ["track"] != "pinned" || typ["title"] != "Card" {
					t.Errorf("%s deck type = %v", target, site["type"])
				}
			case "plain":
				if ok {
					t.Errorf("%s plain carries a type: %v", target, typ)
				}
			}
		}
	}
}

func TestTypedSiteDownload(t *testing.T) {
	env := typedDeckEnv(t)
	rec := env.get(deckHost, "/api/download")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Spot-Type") != "card@1.0.0" {
		t.Fatalf("download = %d %v", rec.Code, rec.Header())
	}
	zr, _ := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if len(zr.File) != 1 || zr.File[0].Name != "card.json" {
		t.Fatalf("download entries = %v", zr.File)
	}
	env.mustDeploy("plain", map[string]string{"index.html": "x"}, nil)
	if rec := env.get("plain.spot.localhost", "/api/download"); rec.Header().Get("X-Spot-Type") != "" {
		t.Fatal("untyped download carries X-Spot-Type")
	}
}

func TestCloudflareRefusesTypedSites(t *testing.T) {
	env := typedDeckEnv(t)
	eligibility := env.srv.cloudflareEligibilityForSite(t.Context(), "deck", cloudflareSnapshot{})
	if eligibility.Eligible || len(eligibility.Reasons) == 0 || !strings.Contains(strings.Join(eligibility.Reasons, " "), "typed sites") {
		t.Fatalf("typed eligibility = %+v", eligibility)
	}
	env.mustDeploy("plain", map[string]string{"index.html": "x"}, nil)
	for _, reason := range env.srv.cloudflareEligibilityForSite(t.Context(), "plain", cloudflareSnapshot{}).Reasons {
		if strings.Contains(reason, "typed") {
			t.Fatalf("untyped site refused as typed: %s", reason)
		}
	}
}

type repinResponse struct {
	Site     string         `json:"site"`
	Type     map[string]any `json:"type"`
	Previous map[string]any `json:"previous"`
}

type typedSiteList struct {
	Sites []map[string]any `json:"sites"`
}
