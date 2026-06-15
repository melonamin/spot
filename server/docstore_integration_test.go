//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Integration tests run against SQLite, using a temp database by default:
//
//	just test-integration
func testSQLitePath(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("SPOT_TEST_SQLITE_PATH"); path != "" {
		return path
	}
	return filepath.Join(t.TempDir(), "spot.db")
}

func newTestStore(t *testing.T) *DocStore {
	t.Helper()
	db, err := openSQLiteDB(context.Background(), testSQLitePath(t))
	if err != nil {
		t.Fatalf("open test database: %v", err)
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

	created, err := store.Create(ctx, scope, coll, "", map[string]any{"title": "hello", "n": float64(1)})
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

	time.Sleep(20 * time.Millisecond)
	updated, err := store.Update(ctx, scope, coll, created.ID, map[string]any{"title": "bye"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Data["title"] != "bye" {
		t.Errorf("Update returned data %+v", updated.Data)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("Update timestamp did not advance: %v -> %v", created.UpdatedAt, updated.UpdatedAt)
	}

	docs, err := store.List(ctx, scope, coll, 10, "", "")
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

	doc, err := store.Create(ctx, scopeA, "shared-it-libs", "", map[string]any{"lib": "cursors"})
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

func TestDocStoreOwnership(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const scope, coll = "it-own", "notes"

	alice, err := store.Create(ctx, scope, coll, "alice@example.com", map[string]any{"n": float64(1)})
	if err != nil {
		t.Fatalf("Create alice: %v", err)
	}
	if alice.Owner != "alice@example.com" {
		t.Errorf("created owner = %q, want alice@example.com", alice.Owner)
	}
	if _, err := store.Create(ctx, scope, coll, "bob@example.com", map[string]any{"n": float64(2)}); err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	// An unattributed write keeps an empty owner and is not "mine" for anyone.
	if _, err := store.Create(ctx, scope, coll, "", map[string]any{"n": float64(3)}); err != nil {
		t.Fatalf("Create anon: %v", err)
	}

	all, err := store.List(ctx, scope, coll, 100, "", "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List all returned %d, want 3", len(all))
	}

	mine, err := store.List(ctx, scope, coll, 100, "alice@example.com", "")
	if err != nil {
		t.Fatalf("List mine: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != alice.ID {
		t.Errorf("List owner=alice returned %d docs, want only alice's", len(mine))
	}

	// Reading a document surfaces its owner, and updating preserves it.
	got, err := store.Get(ctx, scope, coll, alice.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("Get owner = %q, want alice@example.com", got.Owner)
	}
	updated, err := store.Update(ctx, scope, coll, alice.ID, map[string]any{"n": float64(9)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Owner != "alice@example.com" {
		t.Errorf("Update owner = %q, want preserved alice@example.com", updated.Owner)
	}
}

func TestDocStoreScopeIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	doc, err := store.Create(ctx, "it-site-a", "notes", "", map[string]any{"secret": true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.Get(ctx, "it-site-b", "notes", doc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope Get: err = %v, want ErrNotFound", err)
	}
	docs, err := store.List(ctx, "it-site-b", "notes", 10, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("cross-scope List returned %d docs, want 0", len(docs))
	}
}
