//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"testing"
)

// Integration tests run against a real PostgreSQL, e.g. the compose one:
//
//	just up
//	just test-integration
func testDSN() string {
	if dsn := os.Getenv("QUICK_TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return "postgres://quick:quick@localhost:5433/quick?sslmode=disable"
}

func newTestStore(t *testing.T) *DocStore {
	t.Helper()
	db, err := openDB(context.Background(), testDSN())
	if err != nil {
		t.Fatalf("connect to test database (is `just up` running?): %v", err)
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(), `DELETE FROM documents WHERE scope LIKE 'it-%'`)
		db.Close()
	})
	return &DocStore{db: db}
}

func TestDocStoreCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const scope, coll = "it-crud", "posts"

	created, err := store.Create(ctx, scope, coll, map[string]any{"title": "hello", "n": float64(1)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.Data["title"] != "hello" {
		t.Fatalf("Create returned %+v", created)
	}

	got, err := store.Get(ctx, scope, coll, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Data["title"] != "hello" || got.Data["n"] != float64(1) {
		t.Errorf("Get returned data %+v", got.Data)
	}

	updated, err := store.Update(ctx, scope, coll, created.ID, map[string]any{"title": "bye"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Data["title"] != "bye" {
		t.Errorf("Update returned data %+v", updated.Data)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("Update did not advance updated_at: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
	}

	docs, err := store.List(ctx, scope, coll, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != created.ID {
		t.Errorf("List returned %d docs, want the created one", len(docs))
	}

	if err := store.Delete(ctx, scope, coll, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, scope, coll, created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, scope, coll, created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete twice: err = %v, want ErrNotFound", err)
	}
}

func TestSharedCollectionsCrossSites(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	scopeA, err := scopeFor("it-site-a", "shared-it-libs")
	if err != nil {
		t.Fatalf("scopeFor: %v", err)
	}
	scopeB, err := scopeFor("it-site-b", "shared-it-libs")
	if err != nil {
		t.Fatalf("scopeFor: %v", err)
	}
	if scopeA != scopeB {
		t.Fatalf("shared scopes differ: %q vs %q", scopeA, scopeB)
	}

	doc, err := store.Create(ctx, scopeA, "shared-it-libs", map[string]any{"lib": "cursors"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { store.Delete(ctx, scopeA, "shared-it-libs", doc.ID) })

	// Written via site A's scope, readable via site B's.
	got, err := store.Get(ctx, scopeB, "shared-it-libs", doc.ID)
	if err != nil {
		t.Fatalf("Get via other site's scope: %v", err)
	}
	if got.Data["lib"] != "cursors" {
		t.Errorf("Get returned data %+v", got.Data)
	}
}

func TestDocStoreScopeIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	doc, err := store.Create(ctx, "it-site-a", "notes", map[string]any{"secret": true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.Get(ctx, "it-site-b", "notes", doc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope Get: err = %v, want ErrNotFound", err)
	}
	docs, err := store.List(ctx, "it-site-b", "notes", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("cross-scope List returned %d docs, want 0", len(docs))
	}
}
