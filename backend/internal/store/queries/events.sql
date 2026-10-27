-- name: AddEvent :one
INSERT INTO events (request_id, target, kind, actor, data)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListRequestEvents :many
SELECT * FROM events WHERE request_id = $1 ORDER BY id;

-- name: ListTargetEvents :many
SELECT * FROM events WHERE target = $1 ORDER BY id DESC LIMIT sqlc.arg(max_rows);
