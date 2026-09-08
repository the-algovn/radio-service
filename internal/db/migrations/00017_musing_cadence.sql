-- +goose Up
-- The musing cadence (spec 2026-09-08-radio-dj-segment-kinds). A musing is a
-- talk break about the night rather than about a song, and it needs a timer of
-- its own because nothing else in the format clock is time-based except the
-- station ID.
--
-- No UPDATE of the singleton row is needed, unlike 00014: a NOT NULL column
-- with a non-volatile default backfills existing rows in place.
ALTER TABLE station
  ADD COLUMN dj_musing_every_min INT NOT NULL DEFAULT 10; -- minutes between musings; 0 disables

-- +goose Down
ALTER TABLE station DROP COLUMN dj_musing_every_min;
