-- +goose Up
-- v1.2 moderation (spec §2, additive half): explicit queue positions and
-- the AI pause switch. The destructive playlist drop is 00008.
ALTER TABLE request ADD COLUMN position INT;  -- NULL = natural (v1) order
ALTER TABLE station ADD COLUMN ai_enabled BOOLEAN NOT NULL DEFAULT TRUE;

-- +goose Down
ALTER TABLE station DROP COLUMN ai_enabled;
ALTER TABLE request DROP COLUMN position;
