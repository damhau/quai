-- name: CreateReview :one
INSERT INTO reviews (id, request_id, reviewer, groups, decision, comment, change_set_hash, reviewer_email, reviewer_auth_time, teams)
VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(sqlc.narg(reviewer_email)::text, ''), sqlc.narg(reviewer_auth_time), sqlc.arg(teams)::text[])
RETURNING *;

-- name: ListReviews :many
SELECT * FROM reviews WHERE request_id = $1 ORDER BY created_at, id;
