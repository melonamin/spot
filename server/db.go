package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteSchemaSQL = `
CREATE TABLE IF NOT EXISTS documents (
    id text PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6)))),
    scope text NOT NULL,
    collection text NOT NULL,
    data text NOT NULL,
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS documents_scope_collection_idx
    ON documents (scope, collection, created_at DESC);

CREATE TABLE IF NOT EXISTS sites (
    name text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE TABLE IF NOT EXISTS site_deploy_audit (
    id integer PRIMARY KEY AUTOINCREMENT,
    site text NOT NULL,
    actor_email text NOT NULL DEFAULT '',
    actor_peer_ip text NOT NULL DEFAULT '',
    actor_name text NOT NULL DEFAULT '',
    actor_groups text NOT NULL DEFAULT '[]',
    action text NOT NULL,
    status text NOT NULL,
    file_count integer NOT NULL DEFAULT 0,
    total_bytes integer NOT NULL DEFAULT 0,
    message text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS site_deploy_audit_site_created_idx
    ON site_deploy_audit (site, created_at DESC);
`

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
	return db, nil
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
