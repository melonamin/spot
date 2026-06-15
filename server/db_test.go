package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func documentsHasColumn(ctx context.Context, t *testing.T, db *sql.DB, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('documents')`)
	if err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// TestApplyAdditiveMigrationsAddsOwner pins the upgrade path for databases
// that predate the owner column: the migration adds it, is safe to run
// again, and leaves the store usable.
func TestApplyAdditiveMigrationsAddsOwner(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate a legacy database created before the owner column existed.
	if _, err := db.ExecContext(ctx, `ALTER TABLE documents DROP COLUMN owner`); err != nil {
		t.Fatalf("drop owner column: %v", err)
	}
	if documentsHasColumn(ctx, t, db, "owner") {
		t.Fatal("owner column should be gone after drop")
	}

	// The migration restores it and is idempotent on a second run.
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate twice: %v", err)
	}
	if !documentsHasColumn(ctx, t, db, "owner") {
		t.Fatal("owner column missing after migration")
	}

	store := &DocStore{db: db}
	doc, err := store.Create(ctx, "demo", "notes", "carol@example.com", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("create after migration: %v", err)
	}
	if doc.Owner != "carol@example.com" {
		t.Errorf("owner after migration = %q, want carol@example.com", doc.Owner)
	}
}
