CREATE TABLE IF NOT EXISTS documents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope text NOT NULL,
    collection text NOT NULL,
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_scope_collection_idx
    ON documents (scope, collection, created_at DESC);

CREATE TABLE IF NOT EXISTS sites (
    name text PRIMARY KEY,
    owner_email text NOT NULL DEFAULT '',
    owner_peer_ip text NOT NULL DEFAULT '',
    owner_name text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS site_deploy_audit (
    id bigserial PRIMARY KEY,
    site text NOT NULL,
    actor_email text NOT NULL DEFAULT '',
    actor_peer_ip text NOT NULL DEFAULT '',
    actor_name text NOT NULL DEFAULT '',
    actor_groups jsonb NOT NULL DEFAULT '[]'::jsonb,
    action text NOT NULL,
    status text NOT NULL,
    file_count integer NOT NULL DEFAULT 0,
    total_bytes bigint NOT NULL DEFAULT 0,
    message text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS site_deploy_audit_site_created_idx
    ON site_deploy_audit (site, created_at DESC);
