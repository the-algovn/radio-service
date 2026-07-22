-- +goose Up
-- v1.1 on-air provenance (spec 2026-07-22-radio-v11-dj-explains-picks).
ALTER TABLE request ADD COLUMN reason TEXT NOT NULL DEFAULT '';
-- Denormalized at air time, like title/artist: history must survive
-- library deletes AND request-table pruning. Old rows read as unattributed.
ALTER TABLE air_log ADD COLUMN source            TEXT NOT NULL DEFAULT '';
ALTER TABLE air_log ADD COLUMN requested_by_name TEXT NOT NULL DEFAULT '';
ALTER TABLE air_log ADD COLUMN reason            TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE air_log DROP COLUMN IF EXISTS reason;
ALTER TABLE air_log DROP COLUMN IF EXISTS requested_by_name;
ALTER TABLE air_log DROP COLUMN IF EXISTS source;
ALTER TABLE request DROP COLUMN IF EXISTS reason;
