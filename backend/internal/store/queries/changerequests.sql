-- ServiceNow change requests (migrations/00031_change_requests.sql). Instants come from the
-- caller's clock, never now(), so that tests drive them.

-- name: CountChangeRequests :one
SELECT count(*) FROM change_requests WHERE request_id = $1;

-- name: CreateChangeRequest :one
INSERT INTO change_requests (id, request_id, generation, correlation_id, change_set_hash, kind, template, created_at)
VALUES (sqlc.arg(id), sqlc.arg(request_id), sqlc.arg(generation), sqlc.arg(correlation_id), sqlc.arg(change_set_hash), sqlc.arg(kind),
        sqlc.arg(template), sqlc.arg(created_at))
RETURNING *;

-- name: GetChangeRequest :one
SELECT * FROM change_requests WHERE id = $1;

-- name: LockChangeRequest :one
-- A change request, its row locked until the transaction ends; taken after its request's lock.
SELECT * FROM change_requests WHERE id = $1 FOR UPDATE;

-- name: CurrentChangeRequest :one
-- The request's change request that is not superseded, if any.
SELECT * FROM change_requests WHERE request_id = $1 AND superseded_at IS NULL;

-- name: ListChangeRequests :many
-- A request's change requests, the current (newest) first.
SELECT * FROM change_requests WHERE request_id = $1 ORDER BY generation DESC;

-- name: SupersedeChangeRequest :one
-- Supersedes the request's current change request: its content changed.
UPDATE change_requests SET superseded_at = sqlc.arg(at)
WHERE request_id = sqlc.arg(request_id) AND superseded_at IS NULL
RETURNING *;

-- name: SetChangeRequestOpened :execrows
-- What ServiceNow answered for a change request Quai created or found: its sys_id, once.
UPDATE change_requests SET sys_id = sqlc.arg(sys_id), number = sqlc.arg(number), state = sqlc.arg(state), state_label = sqlc.arg(state_label),
    approval = sqlc.arg(approval), opened_at = sqlc.arg(at), polled_at = sqlc.arg(at), next_poll_at = NULL, failures = 0, failure = '',
    failing_since = NULL
WHERE id = sqlc.arg(id) AND sys_id IS NULL;

-- name: SetChangeRequestPrepared :exec
-- Its long fields are written and it was taken for approval, or ServiceNow refused that (problem).
UPDATE change_requests SET prepared_at = sqlc.arg(at), problem = sqlc.arg(problem), next_poll_at = NULL, failures = 0, failure = '',
    failing_since = NULL
WHERE id = sqlc.arg(id);

-- name: SetChangeRequestPolled :exec
-- What ServiceNow reported of a change request now, and when to read it next.
UPDATE change_requests SET state = sqlc.arg(state), state_label = sqlc.arg(state_label), approval = sqlc.arg(approval),
    outcome = sqlc.arg(outcome), polled_at = sqlc.arg(at), next_poll_at = sqlc.arg(next_poll_at), failures = 0, failure = '',
    failing_since = NULL
WHERE id = sqlc.arg(id);

-- name: SetChangeRequestFailed :exec
-- A call about a change request failed: why, and when to try again.
UPDATE change_requests SET failures = failures + 1, failure = sqlc.arg(failure), failing_since = COALESCE(failing_since, sqlc.arg(at)),
    next_poll_at = sqlc.arg(next_poll_at)
WHERE id = sqlc.arg(id);

-- name: DueChangeRequests :many
-- The current change requests that are due, the most overdue first: to open or to read, those of
-- requests pending approval, or approved and not merging yet, without break-glass; to close, those
-- whose content was applied or whose apply failed, until Quai is done.
SELECT c.* FROM change_requests c JOIN requests r ON r.id = c.request_id
WHERE c.superseded_at IS NULL
  AND (r.state IN ('pending_approval', 'approved') AND r.break_glass_at IS NULL OR c.close_requested_at IS NOT NULL AND c.closed_at IS NULL)
  AND (c.next_poll_at IS NULL OR c.next_poll_at <= sqlc.arg(now))
ORDER BY c.next_poll_at NULLS FIRST, c.id
LIMIT sqlc.arg(max_rows);

-- name: SetChangeRequestClosing :execrows
-- The change request is to be closed, once: its content was applied (successful) or its apply
-- failed (unsuccessful). It is due at once, its earlier failures and problem forgotten.
UPDATE change_requests SET close_outcome = sqlc.arg(close_outcome), close_requested_at = sqlc.arg(at), evidence_token = sqlc.arg(evidence_token),
    next_poll_at = NULL, failures = 0, failure = '', failing_since = NULL, problem = ''
WHERE id = sqlc.arg(id) AND superseded_at IS NULL AND close_requested_at IS NULL;

-- name: SetChangeRequestClosed :exec
-- Quai is done closing the change request: how (close_result), what ServiceNow reported last, and
-- what someone must do (problem).
UPDATE change_requests SET closed_at = sqlc.arg(at), close_result = sqlc.arg(close_result), problem = sqlc.arg(problem),
    state = sqlc.arg(state), state_label = sqlc.arg(state_label), approval = sqlc.arg(approval), polled_at = sqlc.arg(at), next_poll_at = NULL,
    failures = 0, failure = '', failing_since = NULL
WHERE id = sqlc.arg(id) AND closed_at IS NULL;

-- name: AddChangeRequestAttachment :exec
-- A file of the evidence is about to be attached: its size and hash, as sent; those of a file
-- attached already stay.
INSERT INTO change_request_attachments (change_request_id, name, bytes, sha256)
VALUES (sqlc.arg(change_request_id), sqlc.arg(name), sqlc.arg(bytes), sqlc.arg(sha256))
ON CONFLICT (change_request_id, name) DO UPDATE SET bytes = EXCLUDED.bytes, sha256 = EXCLUDED.sha256
WHERE change_request_attachments.sys_id IS NULL;

-- name: SetChangeRequestAttached :exec
-- A file of the evidence is attached: the sys_id ServiceNow answered, or a lookup by its name found.
UPDATE change_request_attachments SET sys_id = sqlc.arg(sys_id), attached_at = sqlc.arg(at)
WHERE change_request_id = sqlc.arg(change_request_id) AND name = sqlc.arg(name) AND sys_id IS NULL;

-- name: ListChangeRequestAttachments :many
SELECT * FROM change_request_attachments WHERE change_request_id = $1 ORDER BY name;
