-- name: CreateRequest :one
INSERT INTO requests (id, kind, action, target, type_name, blueprint, requester, requester_email, reason, inputs, state,
                      definitions_commit, base_commit, stack, rules, owners, stacks, codeowners, codeowners_unmatched, step_up,
                      manager, manager_reason, separation, submitted_at, expiry, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, COALESCE(sqlc.narg(stacks)::text[], '{}'),
        COALESCE(sqlc.narg(codeowners)::text[], '{}'), COALESCE(sqlc.narg(codeowners_unmatched)::text[], '{}'), sqlc.arg(step_up),
        sqlc.arg(manager), sqlc.arg(manager_reason), COALESCE(sqlc.narg(separation)::jsonb, '{}'),
        COALESCE(sqlc.narg(submitted_at)::timestamptz, now()), sqlc.arg(expiry), sqlc.narg(expires_at))
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

-- name: SetRequestRules :exec
-- The rules of a request after a plan decided those that read it.
UPDATE requests SET rules = $2, updated_at = now() WHERE id = $1;

-- name: ApplyingRequests :many
-- The requests of a repository waiting for an apply of stack.
SELECT r.* FROM requests r
JOIN change_sets cs ON cs.request_id = r.id
WHERE r.state = 'applying' AND cs.repository = sqlc.arg(repository) AND sqlc.arg(stack)::text = ANY(r.stacks);

-- name: LockRequest :one
-- The request, its row locked until the transaction ends: what decides on a request that waits
-- for approval (a review, its expiry, a reminder) reads its state under the lock.
SELECT * FROM requests WHERE id = $1 FOR UPDATE;

-- name: ListPendingApproval :many
-- The requests that wait for approval, the longest waiting first: those reminders, escalation
-- and expiry may be due for.
SELECT * FROM requests WHERE state = 'pending_approval' ORDER BY submitted_at, id;

-- name: SetRequestReminded :exec
UPDATE requests SET reminded_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: SetRequestEscalated :exec
UPDATE requests SET escalated_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: SetRequestExpiryWarned :exec
UPDATE requests SET expiry_warned_at = sqlc.arg(at) WHERE id = sqlc.arg(id);
