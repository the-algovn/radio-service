-- +goose Up
-- The request queue + request record in one table (spec §3): a listener
-- request or an AI pick, statuses approved → ready → aired | failed. Named
-- per products/radio/architecture.md so the dedication work later extends it.
-- yt_id is deliberately NOT an FK to track: the request usually predates the
-- library row (the ingest worker creates the track); a request whose track
-- vanishes later fails at air time and is skipped.
CREATE TABLE request (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source        TEXT NOT NULL,              -- 'listener' | 'ai'
    requested_by  TEXT NOT NULL DEFAULT '',   -- JWT sub; '' for source='ai'
    display_name  TEXT NOT NULL DEFAULT '',   -- server-derived, never client-supplied
    yt_id         TEXT NOT NULL,
    title         TEXT NOT NULL,
    channel       TEXT NOT NULL,
    duration_s    BIGINT NOT NULL,
    thumbnail_url TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,              -- 'approved' | 'ready' | 'aired' | 'failed'
    fail_reason   TEXT NOT NULL DEFAULT '',
    attempts      INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    aired_at      TIMESTAMPTZ
);
CREATE INDEX request_queue_idx ON request (status, source, created_at);
CREATE INDEX request_user_idx  ON request (requested_by, created_at);

-- +goose Down
DROP TABLE IF EXISTS request;
