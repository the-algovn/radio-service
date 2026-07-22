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

-- Station queries below use extra ::text casts that aren't semantically
-- necessary in plain Postgres: sqlc's static analyzer (no live DB
-- connection configured) can't otherwise infer a concrete Go type for
-- COALESCE(uuid_col::text, '') or for a bare uuid-column parameter, and
-- falls back to interface{}/pgtype.UUID instead of the intended string.
-- The outer ::text on the COALESCE reads (SELECT/RETURNING) and the
-- ::text::uuid round-trip on the SetStationActive param are cast
-- workarounds only — behavior is unchanged from a plain uuid column.
-- name: GetStationRow :one
SELECT COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since
FROM station WHERE id = TRUE;

-- name: LockStationRow :one
SELECT COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since
FROM station WHERE id = TRUE FOR UPDATE;

-- name: SetStationActive :exec
UPDATE station SET active_playlist_id = sqlc.arg(active_playlist_id)::text::uuid, updated_at = now() WHERE id = TRUE;

-- name: StationGoOnAir :one
UPDATE station SET on_air = TRUE, on_air_since = now(), updated_at = now() WHERE id = TRUE
RETURNING COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since;

-- name: StationGoOffAir :one
UPDATE station SET on_air = FALSE, on_air_since = NULL, updated_at = now() WHERE id = TRUE
RETURNING COALESCE(active_playlist_id::text, '')::text AS active_playlist_id, on_air, on_air_since;

-- name: AppendAirLog :exec
INSERT INTO air_log (yt_id, title, artist, started_at, duration_s)
VALUES ($1, $2, $3, $4, $5);

-- name: LatestAirLog :one
SELECT id, yt_id, title, artist, started_at, duration_s
FROM air_log ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: AirHistory :many
SELECT yt_id, title, artist, started_at, duration_s
FROM air_log
WHERE started_at + make_interval(secs => duration_s) < now()
ORDER BY started_at DESC
LIMIT $1;

-- name: BeatListener :exec
INSERT INTO radio_listener (session_id, last_seen) VALUES ($1, now())
ON CONFLICT (session_id) DO UPDATE SET last_seen = now();

-- name: CountListeners :one
SELECT count(*) FROM radio_listener
WHERE last_seen > now() - interval '75 seconds';

-- name: PruneListeners :exec
DELETE FROM radio_listener WHERE last_seen < now() - interval '10 minutes';

-- name: CreateRequest :one
INSERT INTO request (source, requested_by, display_name, yt_id, title, channel, duration_s, thumbnail_url, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id::text AS id, source, requested_by, display_name, yt_id, title, channel,
          duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at;

-- Air order: listener requests FIFO, then AI picks FIFO — (source = 'ai')
-- sorts false (listener) before true.
-- name: NextReadyRequest :one
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at
FROM request WHERE status = 'ready'
ORDER BY (source = 'ai'), created_at, id LIMIT 1;

-- name: OldestApprovedRequest :one
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at
FROM request WHERE status = 'approved'
ORDER BY created_at, id LIMIT 1;

-- name: PendingRequests :many
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at
FROM request WHERE status IN ('approved', 'ready')
ORDER BY (source = 'ai'), created_at, id;

-- name: RequestsByUser :many
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at
FROM request WHERE requested_by = $1
ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: CountPendingByUser :one
SELECT count(*) FROM request
WHERE requested_by = $1 AND status IN ('approved', 'ready');

-- name: CountRequestsSince :one
SELECT count(*) FROM request WHERE requested_by = $1 AND created_at >= $2;

-- name: HasPendingYTID :one
SELECT EXISTS(
  SELECT 1 FROM request WHERE yt_id = $1 AND status IN ('approved', 'ready')
);

-- name: MarkRequestReady :execrows
UPDATE request SET status = 'ready' WHERE id = $1 AND status = 'approved';

-- name: MarkRequestAired :execrows
UPDATE request SET status = 'aired', aired_at = $2 WHERE id = $1;

-- name: MarkRequestFailed :execrows
UPDATE request SET status = 'failed', fail_reason = $2 WHERE id = $1;

-- name: BumpRequestAttempts :one
UPDATE request SET attempts = attempts + 1, fail_reason = $2 WHERE id = $1
RETURNING attempts;

-- name: AiredSince :one
SELECT EXISTS(
  SELECT 1 FROM air_log WHERE yt_id = $1 AND started_at >= $2
);

-- name: RecentAirLogYTIDs :many
SELECT yt_id FROM air_log ORDER BY started_at DESC, id DESC LIMIT $1;

-- name: AllTrackIDs :many
SELECT yt_id FROM track ORDER BY yt_id;

-- name: SumLedgerCostSince :one
SELECT COALESCE(SUM(cost_usd), 0)::double precision AS total
FROM ledger_line WHERE ts >= $1;
