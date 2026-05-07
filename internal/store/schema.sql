CREATE TABLE IF NOT EXISTS jobs (
  id                    TEXT PRIMARY KEY,
  image                 TEXT NOT NULL,
  command               TEXT NOT NULL,
  args_json             TEXT NOT NULL,
  state                 TEXT NOT NULL,
  created_at            INTEGER NOT NULL,
  started_at            INTEGER,
  finished_at           INTEGER,
  exit_code             INTEGER,
  last_checkpoint_at    INTEGER,
  last_checkpoint_path  TEXT,
  last_checkpoint_epoch INTEGER,
  last_checkpoint_found INTEGER,
  container_id          TEXT,
  controller_pid        INTEGER,
  state_volume_path     TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);

CREATE TABLE IF NOT EXISTS job_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL,
  recorded_at INTEGER NOT NULL,
  FOREIGN KEY (job_id) REFERENCES jobs(id)
);

CREATE INDEX IF NOT EXISTS idx_events_job ON job_events(job_id, id);

-- Single-row table tracking the most recent WAL checkpoint, exposed by /healthz.
CREATE TABLE IF NOT EXISTS wal_marker (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  last_checkpoint INTEGER NOT NULL
);
INSERT OR IGNORE INTO wal_marker(id, last_checkpoint) VALUES (1, 0);
