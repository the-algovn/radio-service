-- +goose Up
-- The show timeline (spec 2026-08-04-radio-console-show-timeline). Three
-- additive changes; nothing on the air path reads any of them.
--
--   1. talk_segment — what the MC actually AIRED. talk_memory (00014) cannot
--      answer this: it is written at PREPARE time, and prepared clips are
--      routinely discarded (stale anchor, operator pause, off air), so it
--      structurally over-reports.
--   2. broadcast_session — session boundaries as facts. station.on_air_since
--      holds only the CURRENT session and can never place a historical divider.
--   3. llm_call.correlation_id — links an aired talk segment to the call(s)
--      that scripted it. The audit row is written by an Eino global callback
--      that never sees the director, so ctx propagation is the only link.
CREATE TABLE talk_segment (
    id             BIGSERIAL PRIMARY KEY,
    kind           TEXT        NOT NULL,   -- 'seam' | 'station_id' (live.ClipSeam / ClipStationID)
    started_at     TIMESTAMPTZ NOT NULL,
    duration_s     INT         NOT NULL,
    script         TEXT        NOT NULL DEFAULT '',
    backsell_title TEXT        NOT NULL DEFAULT '',
    promise_title  TEXT        NOT NULL DEFAULT '',
    correlation_id TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX talk_segment_started_at_idx ON talk_segment (started_at DESC);

CREATE TABLE broadcast_session (
    id         BIGSERIAL PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ              -- NULL = open
);
CREATE INDEX broadcast_session_started_at_idx ON broadcast_session (started_at DESC);

ALTER TABLE llm_call ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';
CREATE INDEX llm_call_correlation_idx ON llm_call (correlation_id) WHERE correlation_id <> '';

-- +goose Down
DROP INDEX IF EXISTS llm_call_correlation_idx;
ALTER TABLE llm_call DROP COLUMN IF EXISTS correlation_id;
DROP TABLE IF EXISTS broadcast_session;
DROP TABLE IF EXISTS talk_segment;
