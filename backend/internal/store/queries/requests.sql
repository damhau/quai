-- name: CreateRequest :one
INSERT INTO requests (id, kind, action, target, type_name, blueprint, requester, requester_email, reason, inputs, state,
                      definitions_commit, base_commit, stack, rules, owners, stacks, codeowners, codeowners_unmatched, step_up,
                      manager, manager_reason, separation, submitted_at, expiry, expires_at, window_subjects)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, COALESCE(sqlc.narg(stacks)::text[], '{}'),
        COALESCE(sqlc.narg(codeowners)::text[], '{}'), COALESCE(sqlc.narg(codeowners_unmatched)::text[], '{}'), sqlc.arg(step_up),
        sqlc.arg(manager), sqlc.arg(manager_reason), COALESCE(sqlc.narg(separation)::jsonb, '{}'),
        COALESCE(sqlc.narg(submitted_at)::timestamptz, now()), sqlc.arg(expiry), sqlc.narg(expires_at),
        sqlc.narg(window_subjects)::jsonb)
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
-- concurrent transitions cannot both succeed. A request back in pending approval (its content
-- changed, a new plan) is no longer broken: its break-glass was for the content it recorded, so
-- its approvals and its change windows apply again (migrations/00030_break_glass.sql).
UPDATE requests SET state = sqlc.arg(to_state), updated_at = now(),
    break_glass_at = CASE WHEN sqlc.arg(to_state) = 'pending_approval' THEN NULL ELSE break_glass_at END,
    break_glass_by = CASE WHEN sqlc.arg(to_state) = 'pending_approval' THEN '' ELSE break_glass_by END
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

-- name: ListApprovedRequests :many
-- The requests approved and not delivered yet: those the change windows may hold or release.
SELECT * FROM requests WHERE state IN ('approved', 'merging') ORDER BY updated_at, id;

-- name: SetRequestWindowHeld :exec
-- Holds a request for its change windows (at, with the reason quai/approval gives), or releases
-- it (NULL, '').
UPDATE requests SET window_held_at = sqlc.narg(at), window_reason = sqlc.arg(reason) WHERE id = sqlc.arg(id);

-- name: SetRequestBreakGlass :one
-- Marks a request, approved, as broken by a member of the break-glass role: it merges without its
-- approvals and whatever its change windows say.
UPDATE requests SET break_glass_at = sqlc.arg(at), break_glass_by = sqlc.arg(breaker), updated_at = now()
WHERE id = sqlc.arg(id) AND state = 'approved'
RETURNING *;

-- name: PendingReviewedBy :many
-- The requests waiting for approval that a person reviewed, or that were reviewed on their behalf,
-- with a review not voided.
SELECT r.* FROM requests r
WHERE r.state = 'pending_approval' AND EXISTS (
    SELECT 1 FROM reviews v WHERE v.request_id = r.id AND v.voided_at IS NULL
      AND (v.reviewer = sqlc.arg(name) OR v.on_behalf_of = sqlc.arg(name)))
ORDER BY r.submitted_at, r.id;

-- name: ListEvidenceRequests :many
-- Requests created in a period, oldest first, of a repository, in a state and broken by
-- break-glass when asked: an auditor's sample, with each request's repository (its first change
-- set's, as ListChangeSets orders them) and whether glass was broken for it.
SELECT sqlc.embed(r),
  COALESCE((SELECT c.repository FROM change_sets c WHERE c.request_id = r.id ORDER BY c.repository LIMIT 1), '')::text AS repository,
  EXISTS (SELECT 1 FROM break_glass b WHERE b.request_id = r.id) AS broken
FROM requests r
WHERE r.created_at >= sqlc.arg(since) AND r.created_at < sqlc.arg(until)
  AND (sqlc.arg(state)::text = '' OR r.state = sqlc.arg(state)::text)
  AND (sqlc.arg(repository)::text = '' OR EXISTS (SELECT 1 FROM change_sets c WHERE c.request_id = r.id AND c.repository = sqlc.arg(repository)::text))
  AND (NOT sqlc.arg(break_glass)::bool OR EXISTS (SELECT 1 FROM break_glass b WHERE b.request_id = r.id))
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(max_rows);
