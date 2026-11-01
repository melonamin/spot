package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type interruptedTypeDeleteStore struct {
	*LocalSiteStore
	interrupt bool
}

func (s *interruptedTypeDeleteStore) DeleteTypeFiles(ctx context.Context, name string) error {
	if s.interrupt {
		s.interrupt = false
		return errors.New("storage offline")
	}
	return s.LocalSiteStore.DeleteTypeFiles(ctx, name)
}

func TestDeleteTypeCanRetryInterruptedFileCleanup(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.srv.sites = &interruptedTypeDeleteStore{LocalSiteStore: env.srv.sites.(*LocalSiteStore), interrupt: true}
	env.as(typesAdminEmail)
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "storage offline") {
		t.Fatalf("interrupted delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("retry delete = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.root, "_types", "card")); !os.IsNotExist(err) {
		t.Fatalf("type files remain after retry: %v", err)
	}
}

func TestDeleteUnusedTypeRemovesEveryVersionAndFile(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustUpload(cardVersion(t, "1.1.0"))
	env.mustUpload(renamedType(t, "card-other", "1.0.0"))
	env.as(typesAdminEmail)
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.get("spot.localhost", "/api/types/card"); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted type read = %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(env.root, "_types", "card")); !os.IsNotExist(err) {
		t.Fatalf("type files remain: %v", err)
	}
	if rec := env.get("spot.localhost", "/api/types/card-other"); rec.Code != http.StatusOK {
		t.Fatalf("another type was affected: %d", rec.Code)
	}
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("repeat delete = %d %s", rec.Code, rec.Body.String())
	}
	env.mustUpload(cardVersion(t, "1.0.0"))
}

func TestDeleteTypeRefusesReferencedVersions(t *testing.T) {
	for _, state := range []string{"active", "provisioning"} {
		t.Run(state, func(t *testing.T) {
			env := newTypesEnv(t)
			env.mustUpload(cardVersion(t, "1.0.0"))
			env.mustUpload(cardVersion(t, "2.0.0"))
			env.mustDeploy("deck", map[string]string{"card.json": `{}`}, map[string]string{"type": "card@1.0.0"})
			if _, err := env.db.ExecContext(t.Context(), "UPDATE sites SET state = ? WHERE name = 'deck'", state); err != nil {
				t.Fatal(err)
			}
			env.as(typesAdminEmail)
			rec := env.jsonRequest(http.MethodDelete, "/api/types/card", "")
			if rec.Code != http.StatusConflict || errorCode(t, rec) != "type_in_use" {
				t.Fatalf("delete referenced type = %d %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(filepath.Join(env.root, "_types", "card", "1.0.0", "index.html")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeleteTypeRequiresAdminAndPlatformOrigin(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("nonadmin delete = %d", rec.Code)
	}
	env.as(typesAdminEmail)
	req := httptest.NewRequest(http.MethodDelete, "http://spot.localhost/api/types/card", nil)
	req.Header.Set("Authorization", "Bearer publishing-key")
	if rec := env.do(req); rec.Code != http.StatusForbidden {
		t.Fatalf("publishing key delete = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodDelete, "http://deck.spot.localhost/api/types/card", nil)
	if rec := env.do(req); rec.Code != http.StatusBadRequest {
		t.Fatalf("site origin delete = %d", rec.Code)
	}
}

func TestDeletedTypeCannotBeCommittedByAnEarlierDeploy(t *testing.T) {
	env := newTypesEnv(t)
	env.mustUpload(cardVersion(t, "1.0.0"))
	env.mustDeploy("deck", map[string]string{"index.html": "plain"}, nil)
	var generation int64
	if err := env.db.QueryRowContext(t.Context(), "SELECT content_generation FROM sites WHERE name = 'deck'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	env.as(typesAdminEmail)
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	err := env.srv.types.SetDeployedSiteType(t.Context(), "deck", generation, siteTypeRef{Name: "card", Version: "1.0.0", Track: typeTrackPinned})
	if !errors.Is(err, ErrTypePinChanged) {
		t.Fatalf("late pin = %v, want refusal", err)
	}
	if ref := env.siteType("deck"); ref.typed() {
		t.Fatalf("orphan pin: %+v", ref)
	}
}

func TestDeletedTypeCanBeReinstalledWithFreshViewerBytes(t *testing.T) {
	env := newTypesEnv(t)
	files := cardVersion(t, "1.0.0")
	env.mustUpload(files)
	if _, err := env.srv.cachedTypeVersion(t.Context(), "card", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	env.as(typesAdminEmail)
	if rec := env.jsonRequest(http.MethodDelete, "/api/types/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	files["index.html"] = "<h1>Replacement viewer</h1>"
	env.mustUpload(files)
	env.mustDeploy("deck", map[string]string{"card.json": `{}`}, map[string]string{"type": "card@1.0.0"})
	if rec := env.get("deck.spot.localhost", "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Replacement viewer") {
		t.Fatalf("reinstalled viewer = %d %s", rec.Code, rec.Body.String())
	}
}
