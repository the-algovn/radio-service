-- +goose Up
-- Voices are provider-namespaced ids chosen by an operator; no code or schema
-- default names a concrete voice. Existing rows keep their value.
ALTER TABLE station ALTER COLUMN dj_voice_id SET DEFAULT '';

-- +goose Down
ALTER TABLE station ALTER COLUMN dj_voice_id SET DEFAULT 'vi-VN-Neural2-A';
