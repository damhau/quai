-- Break-glass and its post-incident reviews (migrations/00030_break_glass.sql). Instants come
-- from the caller's clock, never now(), so that tests drive them.

-- name: CreateBreakGlass :one
INSERT INTO break_glass (request_id, breaker, breaker_email, auth_time, acr, step_up, reason, incident, change_set_hash, head, from_state, at,
                         incident_check, incident_sys_id, incident_checked_at, incident_failure)
VALUES (sqlc.arg(request_id), sqlc.arg(breaker), sqlc.arg(breaker_email), sqlc.narg(auth_time), sqlc.arg(acr), sqlc.arg(step_up),
        sqlc.arg(reason), sqlc.arg(incident), sqlc.arg(change_set_hash), sqlc.arg(head), sqlc.arg(from_state), sqlc.arg(at),
        sqlc.arg(incident_check), sqlc.arg(incident_sys_id), sqlc.narg(incident_checked_at), sqlc.arg(incident_failure))
RETURNING *;

-- name: SetBreakGlassIncidentChecked :exec
-- What ServiceNow said of the incident, asked again: verified or refuted (with the problem), or a
-- refusal to say (the problem, and it is asked no more).
UPDATE break_glass SET incident_check = sqlc.arg(incident_check), incident_sys_id = sqlc.arg(incident_sys_id),
    incident_checked_at = sqlc.arg(at), incident_problem = sqlc.arg(incident_problem), incident_failure = '', incident_failures = 0,
    incident_next_at = NULL
WHERE request_id = sqlc.arg(request_id) AND incident_check = 'unverified';

-- name: SetBreakGlassIncidentLinked :exec
-- The incident's link to the change request: linked, kept, none or refused (with the problem).
UPDATE break_glass SET incident_link = sqlc.arg(incident_link), incident_linked_at = sqlc.arg(at), incident_problem = sqlc.arg(incident_problem),
    incident_failure = '', incident_failures = 0, incident_next_at = NULL
WHERE request_id = sqlc.arg(request_id) AND incident_link = '';

-- name: SetBreakGlassIncidentFailed :exec
-- A check or link of the incident did not reach ServiceNow, or must wait: why, and when to try again.
UPDATE break_glass SET incident_failure = sqlc.arg(incident_failure), incident_failures = incident_failures + 1,
    incident_next_at = sqlc.arg(next_at)
WHERE request_id = sqlc.arg(request_id);

-- name: DueBreakGlassIncidents :many
-- The break-glass records whose incident is due to be checked again or linked, the most overdue
-- first: unverified, or verified and not linked yet, without a problem.
SELECT * FROM break_glass
WHERE (incident_check = 'unverified' OR incident_check = 'verified' AND incident_link = '') AND incident_problem = ''
  AND (incident_next_at IS NULL OR incident_next_at <= sqlc.arg(now))
ORDER BY incident_next_at NULLS FIRST, request_id
LIMIT sqlc.arg(max_rows);

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
