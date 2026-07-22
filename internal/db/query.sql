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

-- name: GetStationRow :one
SELECT on_air, on_air_since, ai_enabled FROM station WHERE id = TRUE;

-- name: LockStationRow :one
SELECT on_air, on_air_since, ai_enabled FROM station WHERE id = TRUE FOR UPDATE;

-- name: StationGoOnAir :one
UPDATE station SET on_air = TRUE, on_air_since = now(), updated_at = now() WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled;

-- name: StationGoOffAir :one
UPDATE station SET on_air = FALSE, on_air_since = NULL, updated_at = now() WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled;

-- name: AppendAirLog :exec
INSERT INTO air_log (yt_id, title, artist, started_at, duration_s, source, requested_by_name, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: LatestAirLog :one
SELECT id, yt_id, title, artist, started_at, duration_s, source, requested_by_name, reason
FROM air_log ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: AirHistory :many
SELECT yt_id, title, artist, started_at, duration_s, source, requested_by_name, reason
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
INSERT INTO request (source, requested_by, display_name, yt_id, title, channel, duration_s, thumbnail_url, status, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id::text AS id, source, requested_by, display_name, yt_id, title, channel,
          duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason;

-- Air order: listener requests FIFO, then AI picks FIFO — (source = 'ai')
-- sorts false (listener) before true.
-- name: NextReadyRequest :one
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason
FROM request WHERE status = 'ready'
ORDER BY position IS NULL, position, (source = 'ai'), created_at, id LIMIT 1;

-- name: OldestApprovedRequest :one
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason
FROM request WHERE status = 'approved'
ORDER BY created_at, id LIMIT 1;

-- name: PendingRequests :many
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason
FROM request WHERE status IN ('approved', 'ready')
ORDER BY position IS NULL, position, (source = 'ai'), created_at, id;

-- name: RequestsByUser :many
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason
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

-- name: SetStationAIEnabled :one
UPDATE station SET ai_enabled = $1, updated_at = now() WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled;

-- name: PendingRequestIDs :many
SELECT id::text AS id FROM request WHERE status IN ('approved', 'ready')
ORDER BY position IS NULL, position, (source = 'ai'), created_at, id;

-- name: SetRequestPosition :exec
UPDATE request SET position = $2 WHERE id = $1;

-- name: RecentTerminalRequests :many
SELECT id::text AS id, source, requested_by, display_name, yt_id, title, channel,
       duration_s, thumbnail_url, status, fail_reason, attempts, created_at, aired_at, reason
FROM request WHERE status IN ('aired', 'failed')
ORDER BY COALESCE(aired_at, created_at) DESC, id DESC
LIMIT $1;

-- name: MarkPendingRequestFailed :execrows
UPDATE request SET status = 'failed', fail_reason = $2
WHERE id = $1 AND status IN ('approved', 'ready');
