CREATE TABLE IF NOT EXISTS documents (
    id text PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6)))),
    scope text NOT NULL,
    collection text NOT NULL,
    owner text NOT NULL DEFAULT '',
    data text NOT NULL,
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

-- Covers both the default newest-first listing and cursor paging (which adds
-- id as a tiebreaker). It subsumes a plain (scope, collection, created_at DESC)
-- index, so only this one is kept; applyAdditiveMigrations drops the older one.
CREATE INDEX IF NOT EXISTS documents_scope_collection_cursor_idx
    ON documents (scope, collection, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS sites (
    name text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    title text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    tags text NOT NULL DEFAULT '[]',
    state text NOT NULL DEFAULT 'active',
    content_dirty integer NOT NULL DEFAULT 0,
    content_generation integer NOT NULL DEFAULT 0,
    policy_transition_generation integer NOT NULL DEFAULT 0,
    policy_previous_hash text NOT NULL DEFAULT '',
    policy_next_hash text NOT NULL DEFAULT '',
    content_external_mutation integer NOT NULL DEFAULT 0,
    content_external_mutation_started_at integer NOT NULL DEFAULT 0,
    content_external_mutation_owner text NOT NULL DEFAULT '',
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
    content_hash text NOT NULL DEFAULT '',
    authorized_as text NOT NULL DEFAULT '',
    auth_method text NOT NULL DEFAULT '',
    publisher_key_id text NOT NULL DEFAULT '',
    publisher_name text NOT NULL DEFAULT '',
    message text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS site_deploy_audit_site_created_idx
    ON site_deploy_audit (site, created_at DESC);

CREATE TABLE IF NOT EXISTS publishing_keys (
    id text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    name text NOT NULL,
    site_prefix text NOT NULL,
    secret_hash blob,
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    last_used_at datetime,
    revoked_at datetime
);

CREATE INDEX IF NOT EXISTS publishing_keys_owner_created_idx
    ON publishing_keys (owner_email, owner_peer_ip, created_at DESC);

CREATE TABLE IF NOT EXISTS site_cloudflare_publications (
    site text PRIMARY KEY REFERENCES sites(name) ON DELETE RESTRICT,
    account_id text NOT NULL DEFAULT '',
    zone_id text NOT NULL DEFAULT '',
    dns_record_id text NOT NULL DEFAULT '',
    dns_managed integer NOT NULL DEFAULT 0,
    project_managed integer NOT NULL DEFAULT 0,
    cleanup_unknown integer NOT NULL DEFAULT 0,
    access_mode text NOT NULL DEFAULT 'public',
    access_emails text NOT NULL DEFAULT '[]',
    requested_access_mode text NOT NULL DEFAULT '',
    requested_access_emails text NOT NULL DEFAULT '[]',
    access_app_id text NOT NULL DEFAULT '',
    access_managed integer NOT NULL DEFAULT 0,
    project_name text NOT NULL,
    hostname text NOT NULL,
    deployment_id text NOT NULL DEFAULT '',
    deployment_url text NOT NULL DEFAULT '',
    content_hash text NOT NULL DEFAULT '',
    file_count integer NOT NULL DEFAULT 0,
    total_bytes integer NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS site_cloudflare_publications_updated_idx
    ON site_cloudflare_publications (updated_at DESC);

-- Older databases may already have the publications table without its foreign
-- key. SQLite cannot add that constraint in place, so this trigger provides the
-- same delete guard on upgrades. The foreign key remains the primary guard for
-- fresh databases.
CREATE TRIGGER IF NOT EXISTS site_cloudflare_publications_prevent_site_delete
BEFORE DELETE ON sites
WHEN EXISTS (
    SELECT 1 FROM site_cloudflare_publications WHERE site = OLD.name
)
BEGIN
    SELECT RAISE(ABORT, 'site has a Cloudflare publication');
END;
