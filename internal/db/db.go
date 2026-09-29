package db

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	tune(cfg)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	warm(ctx, pool, int(cfg.MinConns))
	return pool, nil
}

// tune keeps a pool of already-authenticated connections open. A managed
// Postgres is often a cross-region hop, where opening a connection costs a TCP
// handshake, TLS, and auth — far more than the query itself. Idle connections
// must outlive a user sitting still between clicks.
func tune(cfg *pgxpool.Config) {
	maxConns := int32(runtime.NumCPU() * 4)
	if maxConns < 16 {
		maxConns = 16
	}
	if maxConns > 40 {
		maxConns = 40
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 6
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.MaxConnLifetime = 2 * time.Hour
	cfg.MaxConnLifetimeJitter = 10 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "runex-control-plane"
}

// warm opens the floor of the pool before the first request arrives, so the
// first dashboard load does not pay for the handshakes.
func warm(ctx context.Context, pool *pgxpool.Pool, want int) {
	if want <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conns := make([]*pgxpool.Conn, 0, want)
	for i := 0; i < want; i++ {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			break
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		conn.Release()
	}
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  subtitle TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'idle',
  framework TEXT,
  url TEXT,
  port INT,
  container_name TEXT,
  source_type TEXT,
  source_ref TEXT,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  caption TEXT NOT NULL,
  x DOUBLE PRECISION NOT NULL DEFAULT 0,
  y DOUBLE PRECISION NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'idle',
  container_name TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS nodes_project_id ON nodes(project_id);

CREATE TABLE IF NOT EXISTS deployments (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  phase TEXT NOT NULL,
  source_type TEXT NOT NULL,
  source_ref TEXT,
  framework TEXT,
  url TEXT,
  port INT,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS deployments_project_id ON deployments(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS deploy_logs (
  id BIGSERIAL PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  ts TIMESTAMPTZ NOT NULL DEFAULT now(),
  phase TEXT NOT NULL,
  level TEXT NOT NULL,
  text TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS deploy_logs_project ON deploy_logs(project_id, id);

ALTER TABLE projects ADD COLUMN IF NOT EXISTS languages JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS stack JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS url TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS port INT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS framework TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS source_type TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS source_ref TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS error TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS languages JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS stack JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE IF NOT EXISTS project_vars (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'user',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, key)
);

CREATE INDEX IF NOT EXISTS project_vars_project ON project_vars(project_id, key);

ALTER TABLE project_vars ADD COLUMN IF NOT EXISTS node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE project_vars DROP CONSTRAINT IF EXISTS project_vars_project_id_key_key;
CREATE UNIQUE INDEX IF NOT EXISTS project_vars_project_node_key ON project_vars (project_id, node_id, key);

CREATE TABLE IF NOT EXISTS github_installations (
  id BIGINT PRIMARY KEY,
  account TEXT NOT NULL,
  account_type TEXT NOT NULL DEFAULT 'User',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS github_installation_id BIGINT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS github_branch TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS auto_deploy BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE nodes ALTER COLUMN auto_deploy SET DEFAULT true;
CREATE INDEX IF NOT EXISTS nodes_github_repo ON nodes (source_type, source_ref, github_installation_id);

ALTER TABLE deploy_logs ADD COLUMN IF NOT EXISTS node_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS deploy_logs_node ON deploy_logs (project_id, node_id, id);
CREATE INDEX IF NOT EXISTS deploy_logs_deployment ON deploy_logs (deployment_id, id);
CREATE INDEX IF NOT EXISTS deploy_logs_deployment_ts ON deploy_logs (deployment_id, ts);

ALTER TABLE deployments ADD COLUMN IF NOT EXISTS node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS trigger TEXT NOT NULL DEFAULT 'manual';
CREATE INDEX IF NOT EXISTS deployments_trigger ON deployments(project_id, trigger, created_at DESC);

CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT,
  github_id BIGINT UNIQUE,
  github_login TEXT,
  name TEXT NOT NULL DEFAULT '',
  avatar_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_username_lower ON users (lower(username));
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower ON users (lower(email));

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);

ALTER TABLE projects ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS projects_user_id ON projects(user_id);

ALTER TABLE projects ADD COLUMN IF NOT EXISTS environment TEXT NOT NULL DEFAULT 'production';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS family_id TEXT NOT NULL DEFAULT '';
UPDATE projects SET family_id = id WHERE family_id = '';
CREATE UNIQUE INDEX IF NOT EXISTS projects_family_environment ON projects (family_id, environment);

ALTER TABLE github_installations ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS github_installations_user ON github_installations(user_id);

CREATE TABLE IF NOT EXISTS github_connect_intents (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  return_path TEXT NOT NULL DEFAULT '/deployments',
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS github_connect_intents_user ON github_connect_intents(user_id);
CREATE INDEX IF NOT EXISTS github_connect_intents_expires ON github_connect_intents(expires_at);

CREATE TABLE IF NOT EXISTS github_user_installations (
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  installation_id BIGINT NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, installation_id)
);
CREATE INDEX IF NOT EXISTS github_user_installations_install ON github_user_installations(installation_id);
DROP INDEX IF EXISTS github_user_installations_exclusive;
ALTER TABLE github_installations DROP CONSTRAINT IF EXISTS github_installations_user_id_fkey;
ALTER TABLE github_installations
  ADD CONSTRAINT github_installations_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

INSERT INTO github_user_installations (user_id, installation_id)
SELECT user_id, id FROM github_installations
WHERE user_id IS NOT NULL
ON CONFLICT DO NOTHING;

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS memory_mb integer NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS disk_mb integer NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS platform_url_revoked BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS platform_url_user_set BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS platform_slug TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS nodes_platform_slug_unique
  ON nodes (lower(platform_slug))
  WHERE kind = 'service' AND platform_slug <> '';

UPDATE nodes SET memory_mb = CASE kind
  WHEN 'postgresql' THEN 512
  WHEN 'redis' THEN 128
  ELSE 512
END WHERE memory_mb = 0;
UPDATE nodes SET disk_mb = CASE kind
  WHEN 'postgresql' THEN 2048
  WHEN 'redis' THEN 512
  ELSE 1024
END WHERE disk_mb = 0;

CREATE TABLE IF NOT EXISTS secret_vaults (
  user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  kdf TEXT NOT NULL DEFAULT 'pbkdf2-sha256',
  kdf_iters INT NOT NULL,
  salt TEXT NOT NULL,
  wrap_iv TEXT NOT NULL,
  wrapped_dek TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE secret_vaults ADD COLUMN IF NOT EXISTS unlock_mode TEXT NOT NULL DEFAULT 'passphrase';

CREATE TABLE IF NOT EXISTS secret_groups (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name_iv TEXT NOT NULL,
  name_ct TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS secret_groups_user ON secret_groups (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS secret_items (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  group_id TEXT REFERENCES secret_groups(id) ON DELETE CASCADE,
  body_iv TEXT NOT NULL,
  body_ct TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS secret_items_user ON secret_items (user_id, group_id, created_at DESC);

CREATE TABLE IF NOT EXISTS custom_domains (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'pending',
  token TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  edge_status TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  verified_at TIMESTAMPTZ,
  last_check_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS custom_domains_user ON custom_domains (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS custom_domains_node ON custom_domains (project_id, node_id);
CREATE INDEX IF NOT EXISTS custom_domains_status ON custom_domains (status, last_check_at);
`
