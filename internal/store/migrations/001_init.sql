-- 001_init: core tables. Idempotent.
-- NOTE: IDs are generated in Go (uuid.NewString) and always supplied on INSERT,
-- so no DB extension is required. (CREATE EXTENSION ... IF NOT EXISTS is NOT
-- concurrency-safe when api+worker boot together, so it is deliberately absent.)

CREATE TABLE IF NOT EXISTS assets (
  id UUID PRIMARY KEY,
  status TEXT NOT NULL DEFAULT 'uploaded' CHECK (status IN ('uploaded','validated','rejected','proxy_ready','failed')),
  original_path TEXT NOT NULL,
  proxy_path TEXT,
  duration_ms BIGINT,
  width INT,
  height INT,
  codec TEXT,
  size_bytes BIGINT,
  reject_reason TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS exports (
  id UUID PRIMARY KEY,
  asset_id UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'planned' CHECK (status IN ('planned','rendering','stitching','done','failed','canceled')),
  chunk_count INT NOT NULL DEFAULT 0,
  output_path TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  error TEXT
);

CREATE TABLE IF NOT EXISTS export_chunks (
  id UUID PRIMARY KEY,
  export_id UUID NOT NULL REFERENCES exports(id) ON DELETE CASCADE,
  idx INT NOT NULL,
  start_ms BIGINT NOT NULL,
  end_ms BIGINT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','failed')),
  attempts INT NOT NULL DEFAULT 0,
  worker_id TEXT,
  started_at TIMESTAMPTZ,
  heartbeat_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  output_path TEXT,
  checksum TEXT,
  error TEXT,
  UNIQUE (export_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_chunks_export ON export_chunks(export_id);
CREATE INDEX IF NOT EXISTS idx_chunks_status ON export_chunks(status);
