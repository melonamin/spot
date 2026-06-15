//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

func TestDocStoreQuery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const scope, coll = "it-query", "tasks"

	mk := func(status string, priority float64) {
		if _, err := store.Create(ctx, scope, coll, "", map[string]any{"status": status, "priority": priority}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	mk("open", 3)
	mk("open", 1)
	mk("done", 5)
	mk("open", 2)

	open, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "status", Op: "eq", Value: "open"}},
	})
	if err != nil {
		t.Fatalf("Query eq: %v", err)
	}
	if len(open) != 3 {
		t.Errorf("eq status=open returned %d, want 3", len(open))
	}

	hi, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "priority", Op: "gt", Value: float64(2)}},
	})
	if err != nil {
		t.Fatalf("Query gt: %v", err)
	}
	if len(hi) != 2 {
		t.Errorf("priority>2 returned %d, want 2", len(hi))
	}

	inSet, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "status", Op: "in", Value: []any{"done", "archived"}}},
	})
	if err != nil {
		t.Fatalf("Query in: %v", err)
	}
	if len(inSet) != 1 {
		t.Errorf("status in (done,archived) returned %d, want 1", len(inSet))
	}

	sorted, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "status", Op: "eq", Value: "open"}},
		Sort:  "priority",
		Order: "asc",
	})
	if err != nil {
		t.Fatalf("Query sort: %v", err)
	}
	if len(sorted) != 3 {
		t.Fatalf("sorted returned %d, want 3", len(sorted))
	}
	if sorted[0].Data["priority"] != float64(1) || sorted[2].Data["priority"] != float64(3) {
		t.Errorf("ascending sort wrong: got %v..%v", sorted[0].Data["priority"], sorted[2].Data["priority"])
	}

	n, err := store.Count(ctx, scope, coll, "", []Filter{{Field: "status", Op: "eq", Value: "open"}})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count open = %d, want 3", n)
	}

	if _, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "bad field!", Op: "eq", Value: 1}},
	}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("bad field err = %v, want ErrBadQuery", err)
	}
	if _, err := store.Query(ctx, scope, coll, ListQuery{
		Where: []Filter{{Field: "priority", Op: "between", Value: 1}},
	}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("unknown op err = %v, want ErrBadQuery", err)
	}
}

func TestDocStoreGetMany(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const scope, coll = "it-getmany", "items"

	a, err := store.Create(ctx, scope, coll, "", map[string]any{"n": float64(1)})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := store.Create(ctx, scope, coll, "", map[string]any{"n": float64(2)})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if _, err := store.Create(ctx, scope, coll, "", map[string]any{"n": float64(3)}); err != nil {
		t.Fatalf("Create c: %v", err)
	}

	const missing = "00000000-0000-4000-8000-000000000000"
	docs, err := store.GetMany(ctx, scope, coll, []string{a.ID, b.ID, missing})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("GetMany returned %d, want 2 (missing id omitted)", len(docs))
	}

	empty, err := store.GetMany(ctx, scope, coll, nil)
	if err != nil {
		t.Fatalf("GetMany empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("GetMany(nil) returned %d, want 0", len(empty))
	}
}

func TestDocStoreIncrement(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const scope, coll = "it-incr", "counters"

	doc, err := store.Create(ctx, scope, coll, "", map[string]any{"title": "post"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A missing field starts at 0.
	got, err := store.Increment(ctx, scope, coll, doc.ID, "views", 1)
	if err != nil {
		t.Fatalf("Increment new field: %v", err)
	}
	if got.Data["views"] != float64(1) {
		t.Errorf("views = %v, want 1", got.Data["views"])
	}

	got, err = store.Increment(ctx, scope, coll, doc.ID, "views", 4)
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if got.Data["views"] != float64(5) {
		t.Errorf("views = %v, want 5", got.Data["views"])
	}

	// A non-numeric field is rejected and left untouched.
	if _, err := store.Increment(ctx, scope, coll, doc.ID, "title", 1); !errors.Is(err, ErrFieldNotNumeric) {
		t.Errorf("increment string field err = %v, want ErrFieldNotNumeric", err)
	}

	const missing = "00000000-0000-4000-8000-000000000000"
	if _, err := store.Increment(ctx, scope, coll, missing, "views", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("increment missing doc err = %v, want ErrNotFound", err)
	}

	// Ownership is enforced for the owned variant.
	owned, err := store.Create(ctx, scope, coll, "alice@example.com", map[string]any{"n": float64(0)})
	if err != nil {
		t.Fatalf("Create owned: %v", err)
	}
	if _, err := store.IncrementOwned(ctx, scope, coll, owned.ID, "bob@example.com", "n", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("IncrementOwned wrong owner err = %v, want ErrNotFound", err)
	}
	if mine, err := store.IncrementOwned(ctx, scope, coll, owned.ID, "alice@example.com", "n", 2); err != nil {
		t.Fatalf("IncrementOwned: %v", err)
	} else if mine.Data["n"] != float64(2) {
		t.Errorf("owned n = %v, want 2", mine.Data["n"])
	}

	// Concurrent increments must not lose updates.
	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Increment(ctx, scope, coll, doc.ID, "hits", 1); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Increment: %v", err)
	}
	final, err := store.Get(ctx, scope, coll, doc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Data["hits"] != float64(workers) {
		t.Errorf("hits = %v, want %d", final.Data["hits"], workers)
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
