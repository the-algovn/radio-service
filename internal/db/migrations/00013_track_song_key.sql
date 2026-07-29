-- +goose Up
-- A folded (artist, title) identity, so the same recording uploaded as an
-- official MV, a "- Topic" track and a lyric video is recognised as ONE song.
-- Every dedup guard in the station keys on yt_id, which those three defeat.
--
-- '' means NOT COMPUTED, and is not the same as a real key — matching the
-- unmeasured-is-not-zero convention 00012 established for tail cues. Existing
-- rows start uncomputed and are filled as tracks are re-acquired, so this is a
-- quality improvement rather than a deploy prerequisite.
--
-- The index is deliberately NOT UNIQUE. Diacritic folding collapses Vietnamese
-- words that differ only by tone (chờ / chợ / cho all fold to "cho"), so a false
-- merge is expected. Behind a UNIQUE constraint that would become a hard error
-- telling a listener their request is a duplicate when it is not; as a plain
-- index a false merge costs only variety.
ALTER TABLE track ADD COLUMN song_key TEXT NOT NULL DEFAULT '';

CREATE INDEX track_song_key_idx ON track (song_key) WHERE song_key <> '';

-- +goose Down
DROP INDEX IF EXISTS track_song_key_idx;
ALTER TABLE track DROP COLUMN song_key;
