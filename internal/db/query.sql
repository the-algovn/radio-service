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
