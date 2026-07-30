-- +goose Up
-- Seam breaks (spec 2026-07-29-radio-dj-seam-breaks). Three additive changes:
--
--   1. talk_memory — the DJ's show memory. It lived in a process-local ring
--      until now, so every pod restart wiped the night's arc while the feeder
--      happily resumed the music from the air log. Prune-on-write retention,
--      mirroring llm_call (00011).
--   2. next_up.request_id — lets the director pin a REQUEST as the committed
--      next track. Empty string is a shuffle pick, i.e. exactly the 00009
--      behaviour, so commitNextUp needs no change.
--   3. DJ cadence defaults — a longer break every other track, now that one
--      break carries both a backsell and an intro.
CREATE TABLE talk_memory (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT        NOT NULL,
    summary    TEXT        NOT NULL,
    phrases    TEXT[]      NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX talk_memory_created_at_idx ON talk_memory (created_at DESC);

ALTER TABLE next_up ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

ALTER TABLE station
    ALTER COLUMN dj_break_every SET DEFAULT 2,
    ALTER COLUMN dj_max_chars   SET DEFAULT 1500;
UPDATE station SET dj_break_every = 2, dj_max_chars = 1500 WHERE id = TRUE;

-- +goose Down
ALTER TABLE station
    ALTER COLUMN dj_break_every SET DEFAULT 1,
    ALTER COLUMN dj_max_chars   SET DEFAULT 1024;
UPDATE station SET dj_break_every = 1, dj_max_chars = 1024 WHERE id = TRUE;
ALTER TABLE next_up DROP COLUMN request_id;
DROP TABLE talk_memory;
