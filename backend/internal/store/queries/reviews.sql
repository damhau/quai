-- name: CreateReview :one
INSERT INTO reviews (id, request_id, reviewer, groups, decision, comment, change_set_hash, reviewer_email, reviewer_auth_time, teams,
                     reviewer_acr, reviewer_acrs, step_up)
VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(sqlc.narg(reviewer_email)::text, ''), sqlc.narg(reviewer_auth_time), sqlc.arg(teams)::text[],
        sqlc.arg(reviewer_acr), COALESCE(sqlc.arg(reviewer_acrs)::text[], '{}'), sqlc.arg(step_up))
RETURNING *;

-- name: ListReviews :many
SELECT * FROM reviews WHERE request_id = $1 ORDER BY created_at, id;
