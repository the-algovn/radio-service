-- +goose Up
-- v1.2 purge (spec §2, destructive half): the engine has not read playlists
-- since v1. Playlist DATA is deliberately lost; the library keeps every track.
ALTER TABLE station DROP COLUMN active_playlist_id;
DROP TABLE IF EXISTS playlist_item;
DROP TABLE IF EXISTS playlist;

-- +goose Down
CREATE TABLE playlist (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE playlist_item (
    playlist_id UUID NOT NULL REFERENCES playlist(id) ON DELETE CASCADE,
    position    INT  NOT NULL,
    yt_id       TEXT NOT NULL REFERENCES track(yt_id) ON DELETE CASCADE,
    PRIMARY KEY (playlist_id, position),
    UNIQUE (playlist_id, yt_id)
);
ALTER TABLE station ADD COLUMN active_playlist_id UUID REFERENCES playlist(id) ON DELETE SET NULL;
