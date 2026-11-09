-- Break-glass and its post-incident reviews (migrations/00030_break_glass.sql). Instants come
-- from the caller's clock, never now(), so that tests drive them.

-- name: CreateBreakGlass :one
INSERT INTO break_glass (request_id, breaker, breaker_email, auth_time, acr, step_up, reason, incident, change_set_hash, head, from_state, at)
VALUES (sqlc.arg(request_id), sqlc.arg(breaker), sqlc.arg(breaker_email), sqlc.narg(auth_time), sqlc.arg(acr), sqlc.arg(step_up),
        sqlc.arg(reason), sqlc.arg(incident), sqlc.arg(change_set_hash), sqlc.arg(head), sqlc.arg(from_state), sqlc.arg(at))
RETURNING *;

-- name: GetBreakGlass :one
SELECT * FROM break_glass WHERE request_id = $1;

-- name: LockBreakGlass :one
-- A break-glass, its row locked until the transaction ends: sign-offs and reminders of its
-- review are decided under the lock, one after the other.
SELECT * FROM break_glass WHERE request_id = $1 FOR UPDATE;

-- name: ListBreakGlass :many
-- Break-glass records, the open reviews first, then the newest.
SELECT * FROM break_glass
WHERE NOT sqlc.arg(open_only)::boolean OR closed_at IS NULL
ORDER BY closed_at IS NOT NULL, at DESC, request_id
LIMIT sqlc.arg(max_rows);

-- name: ListOpenBreakGlass :many
-- The open reviews, the oldest first: those reminders may be due for.
SELECT * FROM break_glass WHERE closed_at IS NULL ORDER BY at, request_id;

-- name: SetBreakGlassReminded :exec
UPDATE break_glass SET reminded_at = sqlc.arg(at) WHERE request_id = sqlc.arg(request_id);

-- name: CloseBreakGlass :exec
UPDATE break_glass SET closed_at = sqlc.arg(at) WHERE request_id = sqlc.arg(request_id) AND closed_at IS NULL;

-- name: AddBreakGlassSignOff :one
INSERT INTO break_glass_sign_offs (request_id, signer, signer_email, capacity, comment, auth_time, step_up, at)
VALUES (sqlc.arg(request_id), sqlc.arg(signer), sqlc.arg(signer_email), sqlc.arg(capacity), sqlc.arg(comment), sqlc.narg(auth_time),
        sqlc.arg(step_up), sqlc.arg(at))
RETURNING *;

-- name: ListBreakGlassSignOffs :many
SELECT * FROM break_glass_sign_offs WHERE request_id = $1 ORDER BY at, signer;
