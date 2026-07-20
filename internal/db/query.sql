-- name: InsertLedgerLine :exec
INSERT INTO ledger_line (ts, kind, provider, label, chars, in_tokens, out_tokens, cost_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListLedgerLines :many
SELECT ts, kind, provider, label, chars, in_tokens, out_tokens, cost_usd
FROM ledger_line ORDER BY ts ASC;

-- name: SumLedgerCost :one
SELECT COALESCE(SUM(cost_usd), 0)::double precision AS total FROM ledger_line;

-- name: GetTrack :one
SELECT yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, added_at
FROM track WHERE yt_id = $1;

-- name: InsertTrack :exec
INSERT INTO track (yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (yt_id) DO NOTHING;

-- name: ListTracks :many
SELECT yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, added_at
FROM track
WHERE ($1 = '' OR title ILIKE '%' || $1 || '%' OR channel ILIKE '%' || $1 || '%')
ORDER BY added_at DESC
LIMIT $2 OFFSET $3;

-- name: CountTracks :one
SELECT count(*)
FROM track
WHERE ($1 = '' OR title ILIKE '%' || $1 || '%' OR channel ILIKE '%' || $1 || '%');

-- name: DeleteTrack :one
DELETE FROM track WHERE yt_id = $1 RETURNING artifact_id;

-- name: CreatePlaylist :one
INSERT INTO playlist (name) VALUES ($1)
RETURNING id::text AS id, name, created_at, updated_at;

-- name: GetPlaylistRow :one
SELECT id::text AS id, name, created_at, updated_at
FROM playlist WHERE id = $1;

-- name: ListPlaylistRows :many
SELECT p.id::text AS id, p.name, p.created_at, p.updated_at,
       count(pi.yt_id)::int AS track_count,
       COALESCE(sum(t.duration_s), 0)::bigint AS total_duration_s
FROM playlist p
LEFT JOIN playlist_item pi ON pi.playlist_id = p.id
LEFT JOIN track t ON t.yt_id = pi.yt_id
GROUP BY p.id
ORDER BY p.created_at DESC;

-- name: PlaylistStats :one
SELECT count(pi.yt_id)::int AS track_count,
       COALESCE(sum(t.duration_s), 0)::bigint AS total_duration_s
FROM playlist_item pi
JOIN track t ON t.yt_id = pi.yt_id
WHERE pi.playlist_id = $1;

-- name: RenamePlaylist :one
UPDATE playlist SET name = $2, updated_at = now() WHERE id = $1
RETURNING id::text AS id, name, created_at, updated_at;

-- name: DeletePlaylist :execrows
DELETE FROM playlist WHERE id = $1;

-- name: ListPlaylistItems :many
SELECT ((row_number() OVER (ORDER BY pi.position)) - 1)::int AS position,
       pi.yt_id, t.title, t.channel, t.duration_s::bigint AS duration_s
FROM playlist_item pi
JOIN track t ON t.yt_id = pi.yt_id
WHERE pi.playlist_id = $1
ORDER BY pi.position;

-- name: ListPlaylistItemIDs :many
SELECT yt_id FROM playlist_item WHERE playlist_id = $1 ORDER BY position;

-- name: CountPlaylistItems :one
SELECT count(*) FROM playlist_item WHERE playlist_id = $1;

-- name: AppendPlaylistItem :execrows
INSERT INTO playlist_item (playlist_id, position, yt_id)
VALUES ($1,
        COALESCE((SELECT max(position) + 1 FROM playlist_item WHERE playlist_id = $1), 0),
        $2)
ON CONFLICT (playlist_id, yt_id) DO NOTHING;

-- name: DeletePlaylistItem :execrows
DELETE FROM playlist_item WHERE playlist_id = $1 AND yt_id = $2;

-- name: DeleteAllPlaylistItems :exec
DELETE FROM playlist_item WHERE playlist_id = $1;

-- name: InsertPlaylistItemAt :exec
INSERT INTO playlist_item (playlist_id, position, yt_id) VALUES ($1, $2, $3);

-- name: GetStationRow :one
SELECT COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since
FROM station WHERE id = TRUE;

-- name: LockStationRow :one
SELECT COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since
FROM station WHERE id = TRUE FOR UPDATE;

-- name: SetStationActive :exec
UPDATE station SET active_playlist_id = NULLIF(sqlc.arg(active_playlist_id)::text, '')::uuid, updated_at = now() WHERE id = TRUE;

-- name: StationGoOnAir :one
UPDATE station SET on_air = TRUE, on_air_since = now(), updated_at = now() WHERE id = TRUE
RETURNING COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since;

-- name: StationGoOffAir :one
UPDATE station SET on_air = FALSE, on_air_since = NULL, updated_at = now() WHERE id = TRUE
RETURNING COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since;
