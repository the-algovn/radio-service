-- +goose Up
CREATE TABLE playlist (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Ordered membership. A child table (not an array column) keeps FK integrity
-- to track (library deletes cascade out of every playlist) and the dup guard.
CREATE TABLE playlist_item (
    playlist_id UUID NOT NULL REFERENCES playlist(id) ON DELETE CASCADE,
    position    INT  NOT NULL,
    yt_id       TEXT NOT NULL REFERENCES track(yt_id) ON DELETE CASCADE,
    PRIMARY KEY (playlist_id, position),
    UNIQUE (playlist_id, yt_id)
);

-- Singleton station row (id is always TRUE). on_air_since is Slice 2's
-- ear-sync anchor, set by GoOnAir and cleared by GoOffAir.
CREATE TABLE station (
    id                 BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    active_playlist_id UUID REFERENCES playlist(id) ON DELETE SET NULL,
    on_air             BOOLEAN NOT NULL DEFAULT FALSE,
    on_air_since       TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO station (id) VALUES (TRUE);

-- +goose Down
DROP TABLE IF EXISTS station;
DROP TABLE IF EXISTS playlist_item;
DROP TABLE IF EXISTS playlist;
