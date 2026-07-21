-- +goose Up
-- Broadcast air log. Deliberately no FK to track: history must survive
-- library deletes (titles/artists are denormalized at air time). The latest
-- row doubles as the engine's resume anchor after a restart.
CREATE TABLE air_log (
    id         BIGSERIAL PRIMARY KEY,
    yt_id      TEXT NOT NULL,
    title      TEXT NOT NULL,
    artist     TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    duration_s INT NOT NULL
);
CREATE INDEX air_log_started_at_idx ON air_log (started_at DESC);

-- Listener presence from the SPA's 30s heartbeat (per-tab random UUIDs).
CREATE TABLE radio_listener (
    session_id TEXT PRIMARY KEY,
    last_seen  TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS radio_listener;
DROP TABLE IF EXISTS air_log;
