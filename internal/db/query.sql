-- name: InsertLedgerLine :exec
INSERT INTO ledger_line (ts, kind, provider, label, chars, in_tokens, out_tokens, cost_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListLedgerLines :many
SELECT ts, kind, provider, label, chars, in_tokens, out_tokens, cost_usd
FROM ledger_line ORDER BY ts ASC;

-- name: SumLedgerCost :one
SELECT COALESCE(SUM(cost_usd), 0)::double precision AS total FROM ledger_line;

-- name: GetTrack :one
SELECT yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, added_at,
       tail_silence_s, tail_decay_s, song_key
FROM track WHERE yt_id = $1;

-- name: InsertTrack :exec
INSERT INTO track (yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, tail_silence_s, tail_decay_s, song_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (yt_id) DO NOTHING;

-- name: ListTracks :many
SELECT yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, added_at,
       tail_silence_s, tail_decay_s, song_key
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

-- name: UpdateTrackCues :exec
UPDATE track SET tail_silence_s = $2, tail_decay_s = $3 WHERE yt_id = $1;

-- name: ListTracksMissingCues :many
SELECT yt_id, title, channel, duration_s, artifact_id, input_i, input_tp, input_lra, added_at,
       tail_silence_s, tail_decay_s, song_key
FROM track
WHERE tail_silence_s < 0
ORDER BY added_at DESC
LIMIT $1;

-- name: GetStationRow :one
SELECT on_air, on_air_since, ai_enabled,
       dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars
FROM station WHERE id = TRUE;

-- name: LockStationRow :one
SELECT on_air, on_air_since, ai_enabled,
       dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars
FROM station WHERE id = TRUE FOR UPDATE;

-- name: StationGoOnAir :one
UPDATE station SET on_air = TRUE, on_air_since = now(), updated_at = now() WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled,
          dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars;

-- name: StationGoOffAir :one
UPDATE station SET on_air = FALSE, on_air_since = NULL, updated_at = now() WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled,
          dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars;

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
RETURNING on_air, on_air_since, ai_enabled,
          dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars;

-- name: UpdateStationDJSettings :one
UPDATE station SET dj_voice_id = $1, dj_rate = $2, dj_break_every = $3,
       dj_station_id_min = $4, dj_max_chars = $5, updated_at = now()
WHERE id = TRUE
RETURNING on_air, on_air_since, ai_enabled,
          dj_voice_id, dj_rate, dj_break_every, dj_station_id_min, dj_max_chars;

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

-- name: GetNextUp :one
SELECT yt_id, title, channel, request_id FROM next_up WHERE id = TRUE;

-- name: SetNextUp :exec
UPDATE next_up SET yt_id = $1, title = $2, channel = $3, request_id = $4,
       updated_at = now() WHERE id = TRUE;

-- name: ClearNextUp :exec
UPDATE next_up SET yt_id = '', title = '', channel = '', request_id = '',
       updated_at = now() WHERE id = TRUE;

-- name: InsertLLMCall :exec
INSERT INTO llm_call (ts, label, model, provider, system_prompt, user_prompt, output,
                      in_tokens, out_tokens, cost_usd, latency_ms, error, fake)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: ListLLMCalls :many
-- label_filter: "" = all; "script:" = all script:* call-sites (prefix); "programmer:" = all programmer:* call-sites (prefix); else exact match.
SELECT id, ts, label, model, provider, system_prompt, user_prompt, output,
       in_tokens, out_tokens, cost_usd, latency_ms, error, fake
FROM llm_call
WHERE (sqlc.arg(label_filter)::text = ''
       OR label = sqlc.arg(label_filter)
       OR (sqlc.arg(label_filter)::text = 'script:' AND label LIKE 'script:%')
       OR (sqlc.arg(label_filter)::text = 'programmer:' AND label LIKE 'programmer:%'))
  AND (sqlc.arg(errors_only)::bool = false OR error <> '')
ORDER BY ts DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountLLMCalls :one
SELECT count(*) FROM llm_call
WHERE (sqlc.arg(label_filter)::text = ''
       OR label = sqlc.arg(label_filter)
       OR (sqlc.arg(label_filter)::text = 'script:' AND label LIKE 'script:%')
       OR (sqlc.arg(label_filter)::text = 'programmer:' AND label LIKE 'programmer:%'))
  AND (sqlc.arg(errors_only)::bool = false OR error <> '');

-- name: StatsLLMCalls :many
SELECT label, model,
       count(*)::bigint AS n,
       COALESCE(SUM(in_tokens), 0)::bigint AS in_tokens,
       COALESCE(SUM(out_tokens), 0)::bigint AS out_tokens,
       COALESCE(SUM(cost_usd), 0)::double precision AS cost_usd
FROM llm_call
WHERE ts >= sqlc.arg(since)
GROUP BY label, model
ORDER BY cost_usd DESC;

-- name: PruneLLMCalls :exec
DELETE FROM llm_call WHERE ts < sqlc.arg(before);

-- name: InsertTalkMemory :exec
INSERT INTO talk_memory (kind, summary, phrases) VALUES ($1, $2, $3);

-- name: RecentTalkMemory :many
-- Newest first; the caller reverses to get the narrative order.
SELECT id, kind, summary, phrases, created_at
FROM talk_memory
WHERE created_at >= sqlc.arg(created_at)
ORDER BY created_at DESC
LIMIT sqlc.arg(lim);

-- name: PruneTalkMemory :exec
DELETE FROM talk_memory WHERE created_at < $1;
