-- +goose Up
-- Singleton "next up" row (id is always TRUE): the one track the feeder has
-- committed to air next when the request queue is otherwise empty (design
-- 2026-07-23-always-scheduled-next-up). Empty yt_id means "none committed".
-- Always a shuffle pick — listener/AI picks live in the request table.
CREATE TABLE next_up (
    id         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    yt_id      TEXT NOT NULL DEFAULT '',
    title      TEXT NOT NULL DEFAULT '',
    channel    TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO next_up (id) VALUES (TRUE);

-- +goose Down
DROP TABLE IF EXISTS next_up;
