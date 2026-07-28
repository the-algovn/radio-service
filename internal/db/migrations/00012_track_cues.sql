-- +goose Up
-- Transitions Plan 2: per-track tail cues, so a later crossfade can be
-- budgeted per track instead of blindly overlapping a fixed length and
-- chopping the last chord off a cold ending.
--
-- -1 means UNMEASURED, and is not the same as 0. Every existing row starts
-- unmeasured, and the consumer treats unmeasured as "no budget, butt-join" —
-- which is exactly today's behaviour. That is what makes the backfill a
-- quality improvement rather than a deploy prerequisite.
ALTER TABLE track
  ADD COLUMN tail_silence_s DOUBLE PRECISION NOT NULL DEFAULT -1,
  ADD COLUMN tail_decay_s   DOUBLE PRECISION NOT NULL DEFAULT -1;

-- Lets the backfill job find its remaining work without a sequential scan
-- once the library grows.
CREATE INDEX track_missing_cues_idx ON track (added_at) WHERE tail_silence_s < 0;

-- +goose Down
DROP INDEX IF EXISTS track_missing_cues_idx;
ALTER TABLE track
  DROP COLUMN tail_silence_s,
  DROP COLUMN tail_decay_s;
