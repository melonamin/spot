CREATE TABLE IF NOT EXISTS documents (
    id text PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))), 2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6)))),
    scope text NOT NULL,
    collection text NOT NULL,
    owner text NOT NULL DEFAULT '',
    data text NOT NULL,
    created_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')),
    updated_at datetime NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now'))
);

CREATE INDEX IF NOT EXISTS documents_scope_collection_idx
    ON documents (scope, collection, created_at DESC);

CREATE INDEX IF NOT EXISTS documents_scope_collection_cursor_idx
    ON documents (scope, collection, created_at DESC, id DESC);

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
