-- +goose Up
-- The dedication 00005_request anticipated: the note a listener sends with a
-- request for the DJ to read on air.
--
-- Denormalized onto air_log for the same reason 00006_provenance denormalized
-- source/requested_by_name: history must survive both library deletes and
-- request-table pruning. Old rows read as undedicated.
ALTER TABLE request ADD COLUMN dedication TEXT NOT NULL DEFAULT '';
ALTER TABLE air_log ADD COLUMN dedication TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE air_log DROP COLUMN IF EXISTS dedication;
ALTER TABLE request DROP COLUMN IF EXISTS dedication;
