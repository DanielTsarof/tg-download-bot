-- name: CountUserRequestsSince :one
SELECT count(*) FROM requests WHERE user_id = $1 AND created_at >= $2;

-- name: CreateRequest :one
INSERT INTO requests (user_id, kind, query, is_url, format, quality, chat_id, inline_message_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetRequest :one
SELECT * FROM requests WHERE id = $1;

-- name: SetRequestStatusMessage :exec
UPDATE requests SET status_message_id = $2 WHERE id = $1;

-- name: SetRequestTask :exec
UPDATE requests SET task_id = $2, status = $3 WHERE id = $1;

-- name: SetRequestStatus :exec
UPDATE requests SET status = $2 WHERE id = $1 AND status IN ('queued', 'running');

-- name: SucceedRequest :exec
UPDATE requests
SET status = 'succeeded', title = $2, source_id = $3, from_cache = $4,
    error = '', completed_at = now()
WHERE id = $1;

-- name: FailRequest :exec
UPDATE requests
SET status = 'failed', error = $2, completed_at = now()
WHERE id = $1;

-- name: ListUnfinishedRequests :many
SELECT * FROM requests WHERE status IN ('queued', 'running') ORDER BY id;

-- name: ListUserRequests :many
SELECT * FROM requests WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3;

-- name: CountUserRequests :one
SELECT count(*) FROM requests WHERE user_id = $1;

-- name: ListRecentRequests :many
SELECT * FROM requests ORDER BY created_at DESC LIMIT $1 OFFSET $2;

-- name: CountRequests :one
SELECT count(*) FROM requests;

-- name: UserRequestStats :one
SELECT count(*)                                              AS total,
       count(*) FILTER (WHERE status = 'succeeded')          AS succeeded,
       count(*) FILTER (WHERE status = 'failed')             AS failed,
       count(*) FILTER (WHERE status IN ('queued', 'running')) AS in_progress,
       count(*) FILTER (WHERE created_at >= @since::timestamptz) AS today,
       COALESCE(max(created_at), 'epoch')::timestamptz      AS last_request_at
FROM requests
WHERE user_id = $1;

-- name: GetCachedFile :one
SELECT * FROM file_cache WHERE lookup_key = $1 AND format = $2 AND quality = $3;

-- name: PutCachedFile :exec
INSERT INTO file_cache (lookup_key, format, quality, tg_file_id, media_kind, title, source_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (lookup_key, format, quality) DO UPDATE
SET tg_file_id = EXCLUDED.tg_file_id,
    media_kind = EXCLUDED.media_kind,
    title = EXCLUDED.title,
    source_id = EXCLUDED.source_id,
    created_at = now();

-- name: DeleteCachedFile :exec
DELETE FROM file_cache WHERE lookup_key = $1 AND format = $2 AND quality = $3;
