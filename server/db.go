package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteSchemaSQL is the schema applied at startup. schema.sql is the single
// source of truth; editing it changes the live schema.
//
//go:embed schema.sql
var sqliteSchemaSQL string

func openSQLiteDB(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := waitForDB(ctx, db, 5*time.Second); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, sqliteSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// applyAdditiveMigrations brings older databases up to the current schema
// for changes that CREATE TABLE IF NOT EXISTS cannot express: columns added
// to a table that already exists, and dropping an index that a newer one
// supersedes. New installs already match schema.sql, so each step is a no-op
// for them.
func applyAdditiveMigrations(ctx context.Context, db *sql.DB) error {
	if err := ensureColumn(ctx, db, "documents", "owner",
		`ALTER TABLE documents ADD COLUMN owner text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// documents_scope_collection_cursor_idx (scope, collection, created_at DESC,
	// id DESC) serves every lookup the older 3-column documents_scope_collection_idx
	// did, so the latter is redundant write amplification. Drop it on existing
	// databases; schema.sql no longer creates it and applies the cursor index
	// first, so a query is never left without an index. A no-op once dropped.
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS documents_scope_collection_idx`); err != nil {
		return fmt.Errorf("drop redundant documents index: %w", err)
	}
	return nil
}

// ensureColumn adds a column when it is missing. SQLite has no
// "ADD COLUMN IF NOT EXISTS", so the presence check is done against
// pragma_table_info first; running the ALTER unconditionally would fail on
// databases that already have the column.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, ddl string) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("add %s.%s column: %w", table, column, err)
	}
	return nil
}

func waitForDB(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database not reachable after %s: %w", timeout, err)
		}
		time.Sleep(time.Second)
	}
}
