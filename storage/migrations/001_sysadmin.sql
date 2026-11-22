-- Docker Sysadmin v2 extension database (porta E2: data/data.db).
-- Migrated on a copy and backed up with VACUUM INTO by COGNIX[WS].

CREATE TABLE IF NOT EXISTS docker_locks (
  endpoint TEXT NOT NULL,
  kind     TEXT NOT NULL,
  target   TEXT NOT NULL,
  owner    TEXT NOT NULL,
  PRIMARY KEY (endpoint, kind, target)
);

CREATE TABLE IF NOT EXISTS docker_operations (
  id             TEXT PRIMARY KEY,
  request_key    TEXT NOT NULL UNIQUE,
  endpoint       TEXT NOT NULL,
  container_id   TEXT NOT NULL,
  container_name TEXT NOT NULL,
  action         TEXT NOT NULL,
  actor          TEXT NOT NULL,
  agent_id       TEXT NOT NULL DEFAULT '',
  state          TEXT NOT NULL,
  message        TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  finished_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_docker_ops_created ON docker_operations (created_at DESC);

CREATE TABLE IF NOT EXISTS docker_plans (
  id         TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  payload    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS docker_jobs (
  id          TEXT PRIMARY KEY,
  request_key TEXT NOT NULL UNIQUE,
  plan_id     TEXT NOT NULL UNIQUE,
  state       TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  payload     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_docker_jobs_created ON docker_jobs (created_at DESC);

CREATE TABLE IF NOT EXISTS docker_monitors (
  endpoint TEXT NOT NULL,
  project  TEXT NOT NULL,
  revision INTEGER NOT NULL,
  payload  TEXT NOT NULL,
  PRIMARY KEY (endpoint, project)
);

CREATE TABLE IF NOT EXISTS docker_incidents (
  id         TEXT PRIMARY KEY,
  endpoint   TEXT NOT NULL,
  project    TEXT NOT NULL,
  signal     TEXT NOT NULL,
  state      TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  payload    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_docker_incidents_proj ON docker_incidents (endpoint, project, updated_at DESC);

CREATE TABLE IF NOT EXISTS docker_compose_projects (
  id            TEXT PRIMARY KEY,
  workspace_id  TEXT NOT NULL DEFAULT '',
  project       TEXT NOT NULL UNIQUE,
  work_dir      TEXT NOT NULL,
  files_json    TEXT NOT NULL,
  profiles_json TEXT NOT NULL DEFAULT '[]',
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS docker_confirmations (
  id            TEXT PRIMARY KEY,
  revision      TEXT NOT NULL,
  kind          TEXT NOT NULL,
  title         TEXT NOT NULL,
  message       TEXT NOT NULL,
  confirm_label TEXT NOT NULL,
  danger        INTEGER NOT NULL DEFAULT 0,
  payload       TEXT NOT NULL,
  expires_at    TEXT NOT NULL
);
