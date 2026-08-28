package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func tableHasColumn(ctx context.Context, t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
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
	if tableHasColumn(ctx, t, db, "documents", "owner") {
		t.Fatal("owner column should be gone after drop")
	}

	// The migration restores it and is idempotent on a second run.
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate twice: %v", err)
	}
	if !tableHasColumn(ctx, t, db, "documents", "owner") {
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

func TestApplyAdditiveMigrationsAddsCloudflareStateColumns(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, statement := range []string{
		`ALTER TABLE sites DROP COLUMN state`,
		`ALTER TABLE sites DROP COLUMN content_dirty`,
		`ALTER TABLE sites DROP COLUMN content_generation`,
		`ALTER TABLE sites DROP COLUMN policy_transition_generation`,
		`ALTER TABLE sites DROP COLUMN policy_previous_hash`,
		`ALTER TABLE sites DROP COLUMN policy_next_hash`,
		`ALTER TABLE sites DROP COLUMN content_external_mutation`,
		`ALTER TABLE sites DROP COLUMN content_external_mutation_started_at`,
		`ALTER TABLE sites DROP COLUMN content_external_mutation_owner`,
		`ALTER TABLE site_deploy_audit DROP COLUMN content_hash`,
		`ALTER TABLE site_deploy_audit DROP COLUMN authorized_as`,
		`ALTER TABLE site_deploy_audit DROP COLUMN auth_method`,
		`ALTER TABLE site_deploy_audit DROP COLUMN publisher_key_id`,
		`ALTER TABLE site_deploy_audit DROP COLUMN publisher_name`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN account_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN zone_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_record_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN cleanup_unknown`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_managed`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN project_managed`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN access_mode`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN access_emails`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN requested_access_mode`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN requested_access_emails`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN access_app_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN access_managed`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate twice: %v", err)
	}
	for _, tc := range []struct{ table, column string }{
		{"sites", "state"},
		{"sites", "content_dirty"},
		{"sites", "content_generation"},
		{"sites", "policy_transition_generation"},
		{"sites", "policy_previous_hash"},
		{"sites", "policy_next_hash"},
		{"sites", "content_external_mutation"},
		{"sites", "content_external_mutation_started_at"},
		{"sites", "content_external_mutation_owner"},
		{"site_deploy_audit", "content_hash"},
		{"site_deploy_audit", "authorized_as"},
		{"site_deploy_audit", "auth_method"},
		{"site_deploy_audit", "publisher_key_id"},
		{"site_deploy_audit", "publisher_name"},
		{"site_cloudflare_publications", "account_id"},
		{"site_cloudflare_publications", "zone_id"},
		{"site_cloudflare_publications", "dns_record_id"},
		{"site_cloudflare_publications", "cleanup_unknown"},
		{"site_cloudflare_publications", "dns_managed"},
		{"site_cloudflare_publications", "project_managed"},
		{"site_cloudflare_publications", "access_mode"},
		{"site_cloudflare_publications", "access_emails"},
		{"site_cloudflare_publications", "requested_access_mode"},
		{"site_cloudflare_publications", "requested_access_emails"},
		{"site_cloudflare_publications", "access_app_id"},
		{"site_cloudflare_publications", "access_managed"},
	} {
		if !tableHasColumn(ctx, t, db, tc.table, tc.column) {
			t.Fatalf("%s.%s missing after migration", tc.table, tc.column)
		}
	}
}

func TestCloudflareMigrationPreservesLegacyDNSCleanup(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, status) VALUES ('demo', 'spot-demo', 'demo.pages.example.com', 'published')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE site_cloudflare_publications
		SET account_id = 'legacy-account', zone_id = 'legacy-zone' WHERE site = 'demo'`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_record_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN cleanup_unknown`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_managed`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN project_managed`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	var recordID string
	var managed bool
	var projectManaged, cleanupUnknown bool
	if err := db.QueryRowContext(ctx, `SELECT dns_record_id, dns_managed, project_managed, cleanup_unknown
		FROM site_cloudflare_publications WHERE site = 'demo'`).Scan(&recordID, &managed, &projectManaged, &cleanupUnknown); err != nil {
		t.Fatal(err)
	}
	if recordID != "" || !managed || !projectManaged || cleanupUnknown {
		t.Fatalf("migrated cleanup state = DNS id %q managed=%v project managed=%v unknown=%v, want known legacy cleanup enabled",
			recordID, managed, projectManaged, cleanupUnknown)
	}
}

func TestCloudflareMigrationPreservesInterruptedRestrictedIntent(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, status) VALUES ('demo', 'spot-demo', 'demo.pages.example.com', 'restricting')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN requested_access_emails`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN requested_access_mode`); err != nil {
		t.Fatal(err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var mode, emails string
	if err := db.QueryRowContext(ctx, `SELECT requested_access_mode, requested_access_emails
		FROM site_cloudflare_publications WHERE site = 'demo'`).Scan(&mode, &emails); err != nil {
		t.Fatal(err)
	}
	if mode != cloudflareAccessRestricted || emails != "[]" {
		t.Fatalf("migrated requested policy = %q %s, want restricted with allowlist re-entry required", mode, emails)
	}
}

func TestCloudflareMigrationFailsClosedWithoutLegacyLocation(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, status) VALUES ('demo', 'spot-demo', 'demo.pages.example.com', 'published')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE site_cloudflare_publications DROP COLUMN account_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN zone_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_record_id`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN cleanup_unknown`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN dns_managed`,
		`ALTER TABLE site_cloudflare_publications DROP COLUMN project_managed`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	var accountID, zoneID string
	var dnsManaged, projectManaged, cleanupUnknown bool
	if err := db.QueryRowContext(ctx, `SELECT account_id, zone_id, dns_managed, project_managed, cleanup_unknown
		FROM site_cloudflare_publications WHERE site = 'demo'`).Scan(
		&accountID, &zoneID, &dnsManaged, &projectManaged, &cleanupUnknown); err != nil {
		t.Fatal(err)
	}
	if accountID != "" || zoneID != "" || dnsManaged || projectManaged || !cleanupUnknown {
		t.Fatalf("locationless legacy state = account %q zone %q DNS managed=%v project managed=%v unknown=%v, want blocked cleanup",
			accountID, zoneID, dnsManaged, projectManaged, cleanupUnknown)
	}
}

func TestCloudflareMigrationKeepsNewReservationsDNSUnmanaged(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN dns_managed`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN project_managed`); err != nil {
		t.Fatal(err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('new-site', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	if err := repo.InsertReservation(ctx, cloudflarePublication{
		Site: "new-site", ProjectName: "spot-new-site", Hostname: "new-site.pages.example.com", Status: "reserving",
	}); err != nil {
		t.Fatal(err)
	}
	pub, err := repo.Get(ctx, "new-site")
	if err != nil {
		t.Fatal(err)
	}
	if pub == nil || pub.DNSManaged || pub.ProjectManaged {
		t.Fatalf("new reservation after migration = %+v, want DNS and project unmanaged", pub)
	}
}

func TestCloudflareDNSManagedMigrationIsRestartSafe(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, status) VALUES ('demo', 'spot-demo', 'demo.pages.example.com', 'published')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE site_cloudflare_publications SET zone_id = 'legacy-zone' WHERE site = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN dns_managed`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_cloudflare_dns_backfill
		BEFORE UPDATE ON site_cloudflare_publications
		BEGIN SELECT RAISE(ABORT, 'simulated migration interruption'); END`); err != nil {
		t.Fatal(err)
	}

	if err := applyAdditiveMigrations(ctx, db); err == nil {
		t.Fatal("migration unexpectedly succeeded while the legacy backfill was blocked")
	}
	if tableHasColumn(ctx, t, db, "site_cloudflare_publications", "dns_managed") {
		t.Fatal("dns_managed survived a failed backfill; want the schema change rolled back")
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_cloudflare_dns_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("restart migration: %v", err)
	}
	var managed bool
	if err := db.QueryRowContext(ctx, `SELECT dns_managed FROM site_cloudflare_publications WHERE site = 'demo'`).Scan(&managed); err != nil {
		t.Fatal(err)
	}
	if !managed {
		t.Fatal("legacy publication lost DNS cleanup ownership after migration retry")
	}
}

func TestCloudflareProjectManagedMigrationIsRestartSafe(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO sites (name, owner_email) VALUES ('demo', 'alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, status) VALUES ('demo', 'spot-demo', 'demo.pages.example.com', 'published')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE site_cloudflare_publications SET account_id = 'legacy-account' WHERE site = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE site_cloudflare_publications DROP COLUMN project_managed`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_cloudflare_project_backfill
		BEFORE UPDATE ON site_cloudflare_publications
		BEGIN SELECT RAISE(ABORT, 'simulated project migration interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if err := applyAdditiveMigrations(ctx, db); err == nil {
		t.Fatal("project ownership migration unexpectedly succeeded while backfill was blocked")
	}
	if tableHasColumn(ctx, t, db, "site_cloudflare_publications", "project_managed") {
		t.Fatal("project_managed survived a failed backfill; want schema change rolled back")
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_cloudflare_project_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := applyAdditiveMigrations(ctx, db); err != nil {
		t.Fatalf("restart migration: %v", err)
	}
	var managed bool
	if err := db.QueryRowContext(ctx, `SELECT project_managed FROM site_cloudflare_publications WHERE site = 'demo'`).Scan(&managed); err != nil {
		t.Fatal(err)
	}
	if !managed {
		t.Fatal("legacy publication lost project cleanup ownership after migration retry")
	}
}
