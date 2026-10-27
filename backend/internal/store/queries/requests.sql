-- name: CreateRequest :one
INSERT INTO requests (id, kind, action, target, type_name, blueprint, requester, requester_email, reason, inputs, state,
                      definitions_commit, base_commit, stack, rules, owners)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: GetRequest :one
SELECT * FROM requests WHERE id = $1;

-- name: ListRequests :many
-- Requests in the given states (all states when empty), newest first.
SELECT * FROM requests
WHERE (cardinality(sqlc.arg(states)::text[]) = 0 OR state = ANY (sqlc.arg(states)::text[]))
  AND (sqlc.arg(requester)::text = '' OR requester = sqlc.arg(requester)::text)
  AND (sqlc.arg(target)::text = '' OR target = sqlc.arg(target)::text)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(max_rows);

-- name: TransitionRequest :one
-- Moves a request from one state to another; no row when it is not in the expected state, so
-- concurrent transitions cannot both succeed.
UPDATE requests SET state = sqlc.arg(to_state), updated_at = now()
WHERE id = sqlc.arg(id) AND state = sqlc.arg(from_state)
RETURNING *;
