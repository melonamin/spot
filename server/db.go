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
	if err := ensureColumn(ctx, db, "sites", "title",
		`ALTER TABLE sites ADD COLUMN title text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "description",
		`ALTER TABLE sites ADD COLUMN description text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "tags",
		`ALTER TABLE sites ADD COLUMN tags text NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "state",
		`ALTER TABLE sites ADD COLUMN state text NOT NULL DEFAULT 'active'`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "content_dirty",
		`ALTER TABLE sites ADD COLUMN content_dirty integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "content_generation",
		`ALTER TABLE sites ADD COLUMN content_generation integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "policy_transition_generation",
		`ALTER TABLE sites ADD COLUMN policy_transition_generation integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "policy_previous_hash",
		`ALTER TABLE sites ADD COLUMN policy_previous_hash text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "policy_next_hash",
		`ALTER TABLE sites ADD COLUMN policy_next_hash text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "content_external_mutation",
		`ALTER TABLE sites ADD COLUMN content_external_mutation integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "content_external_mutation_started_at",
		`ALTER TABLE sites ADD COLUMN content_external_mutation_started_at integer NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "sites", "content_external_mutation_owner",
		`ALTER TABLE sites ADD COLUMN content_external_mutation_owner text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_deploy_audit", "content_hash",
		`ALTER TABLE site_deploy_audit ADD COLUMN content_hash text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_deploy_audit", "authorized_as",
		`ALTER TABLE site_deploy_audit ADD COLUMN authorized_as text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_deploy_audit", "auth_method",
		`ALTER TABLE site_deploy_audit ADD COLUMN auth_method text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_deploy_audit", "publisher_key_id",
		`ALTER TABLE site_deploy_audit ADD COLUMN publisher_key_id text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_deploy_audit", "publisher_name",
		`ALTER TABLE site_deploy_audit ADD COLUMN publisher_name text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "account_id",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN account_id text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "zone_id",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN zone_id text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "dns_record_id",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN dns_record_id text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureCloudflareCleanupUnknownColumn(ctx, db); err != nil {
		return err
	}
	if err := ensureCloudflareDNSManagedColumn(ctx, db); err != nil {
		return err
	}
	if err := ensureCloudflareProjectManagedColumn(ctx, db); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "access_mode",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN access_mode text NOT NULL DEFAULT 'public'`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "access_emails",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN access_emails text NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "requested_access_mode",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN requested_access_mode text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "requested_access_emails",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN requested_access_emails text NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	// Older interrupted Access creates did not retain the requested policy.
	// Preserve the security-sensitive visibility choice even though the original
	// allowlist must be entered again before the publication can resume.
	if _, err := db.ExecContext(ctx, `UPDATE site_cloudflare_publications
		SET requested_access_mode = 'restricted'
		WHERE status = 'restricting' AND requested_access_mode = ''`); err != nil {
		return fmt.Errorf("backfill requested Cloudflare Access mode: %w", err)
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "access_app_id",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN access_app_id text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "site_cloudflare_publications", "access_managed",
		`ALTER TABLE site_cloudflare_publications ADD COLUMN access_managed integer NOT NULL DEFAULT 0`); err != nil {
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

// ensureCloudflareCleanupUnknownColumn distinguishes old published resources
// whose original account/zone were never stored from new reservations that
// definitely own nothing yet. Unknown cleanup stays blocked for manual repair.
func ensureCloudflareCleanupUnknownColumn(ctx context.Context, db *sql.DB) error {
	exists, err := hasColumn(ctx, db, "site_cloudflare_publications", "cleanup_unknown")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin site_cloudflare_publications.cleanup_unknown migration: %w", err)
	}
	defer tx.Rollback()
	exists, err = hasColumn(ctx, tx, "site_cloudflare_publications", "cleanup_unknown")
	if err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE site_cloudflare_publications ADD COLUMN cleanup_unknown integer NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add site_cloudflare_publications.cleanup_unknown column: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE site_cloudflare_publications SET cleanup_unknown = 1
		WHERE status <> 'reserving' AND (account_id = '' OR zone_id = '')`); err != nil {
		return fmt.Errorf("mark locationless legacy Cloudflare cleanup unknown: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site_cloudflare_publications.cleanup_unknown migration: %w", err)
	}
	return nil
}

// ensureCloudflareDNSManagedColumn preserves cleanup only when the row already
// has its original zone. Locationless legacy rows fail closed rather than
// applying current configuration to resources created elsewhere.
func ensureCloudflareDNSManagedColumn(ctx context.Context, db *sql.DB) error {
	exists, err := hasColumn(ctx, db, "site_cloudflare_publications", "dns_managed")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin site_cloudflare_publications.dns_managed migration: %w", err)
	}
	defer tx.Rollback()

	// Check again inside the transaction so the schema change and legacy-row
	// backfill are one restart-safe unit. SQLite rolls the ALTER TABLE back if
	// the UPDATE or commit fails.
	exists, err = hasColumn(ctx, tx, "site_cloudflare_publications", "dns_managed")
	if err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE site_cloudflare_publications ADD COLUMN dns_managed integer NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add site_cloudflare_publications.dns_managed column: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE site_cloudflare_publications SET dns_managed = 1 WHERE zone_id <> ''`); err != nil {
		return fmt.Errorf("preserve legacy Cloudflare DNS cleanup ownership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site_cloudflare_publications.dns_managed migration: %w", err)
	}
	return nil
}

// ensureCloudflareProjectManagedColumn preserves cleanup only for rows that
// already store their original account. Locationless legacy rows fail closed;
// new rows become owned only after a definite CreateProject success is stored.
func ensureCloudflareProjectManagedColumn(ctx context.Context, db *sql.DB) error {
	exists, err := hasColumn(ctx, db, "site_cloudflare_publications", "project_managed")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin site_cloudflare_publications.project_managed migration: %w", err)
	}
	defer tx.Rollback()
	exists, err = hasColumn(ctx, tx, "site_cloudflare_publications", "project_managed")
	if err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE site_cloudflare_publications ADD COLUMN project_managed integer NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add site_cloudflare_publications.project_managed column: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE site_cloudflare_publications SET project_managed = 1
		 WHERE status <> 'reserving' AND account_id <> ''`); err != nil {
		return fmt.Errorf("preserve legacy Cloudflare project cleanup ownership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site_cloudflare_publications.project_managed migration: %w", err)
	}
	return nil
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func hasColumn(ctx context.Context, db sqlQueryer, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return false, nil
}

// ensureColumn adds a column when it is missing. SQLite has no
// "ADD COLUMN IF NOT EXISTS", so the presence check is done against
// pragma_table_info first; running the ALTER unconditionally would fail on
// databases that already have the column.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, ddl string) error {
	exists, err := hasColumn(ctx, db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
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
