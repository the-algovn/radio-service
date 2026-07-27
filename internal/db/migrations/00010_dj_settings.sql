-- +goose Up
-- DJ voice settings page (spec 2026-07-23): the five RADIO_DJ_* knobs move
-- from env-at-boot to the station row so the director picks up changes per
-- tick. Defaults mirror the CURRENT prod env values (iac 68618e6 / PR #6),
-- so the env→DB cutover is runtime-neutral.
ALTER TABLE station
  ADD COLUMN dj_voice_id       TEXT             NOT NULL DEFAULT 'vi-VN-Neural2-A',
  ADD COLUMN dj_rate           DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  ADD COLUMN dj_break_every    INT              NOT NULL DEFAULT 1,    -- talk break after N tracks; 0 disables
  ADD COLUMN dj_station_id_min INT              NOT NULL DEFAULT 60,   -- minutes between station IDs; 0 disables
  ADD COLUMN dj_max_chars      INT              NOT NULL DEFAULT 1024; -- LLM backsell script rune cap

-- +goose Down
ALTER TABLE station
  DROP COLUMN dj_voice_id,
  DROP COLUMN dj_rate,
  DROP COLUMN dj_break_every,
  DROP COLUMN dj_station_id_min,
  DROP COLUMN dj_max_chars;
