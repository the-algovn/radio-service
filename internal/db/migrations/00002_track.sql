-- +goose Up
CREATE TABLE track (
    yt_id       TEXT PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    channel     TEXT NOT NULL DEFAULT '',
    duration_s  DOUBLE PRECISION NOT NULL,
    artifact_id TEXT NOT NULL,
    input_i     DOUBLE PRECISION NOT NULL,
    input_tp    DOUBLE PRECISION NOT NULL,
    input_lra   DOUBLE PRECISION NOT NULL,
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX track_added_at_idx ON track (added_at DESC);

-- +goose Down
DROP TABLE IF EXISTS track;
