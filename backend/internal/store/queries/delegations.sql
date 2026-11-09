-- Delegations (migrations/00026_delegation.sql). Instants come from the caller's clock, never
-- now(), so that tests drive them.

-- name: LockDelegations :exec
-- Serializes making delegations, between processes: the checks read the other delegations.
SELECT pg_advisory_xact_lock(hashtext('quai.delegations'));

-- name: CreateDelegation :one
INSERT INTO delegations (id, delegator, delegate, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDelegation :one
SELECT * FROM delegations WHERE id = $1;

-- name: DelegationsOf :many
-- The delegations neither revoked nor ended at an instant that people gave or received.
SELECT * FROM delegations
WHERE revoked_at IS NULL AND ends_at > sqlc.arg(at)
  AND (delegator = ANY (sqlc.arg(names)::text[]) OR delegate = ANY (sqlc.arg(names)::text[]))
ORDER BY starts_at, id;

-- name: ListDelegations :many
-- Every delegation neither revoked nor ended at an instant, for platform admins.
SELECT * FROM delegations WHERE revoked_at IS NULL AND ends_at > $1 ORDER BY starts_at, id;

-- name: ActiveDelegations :many
-- The delegations active at an instant.
SELECT * FROM delegations
WHERE revoked_at IS NULL AND starts_at <= $1 AND ends_at > $1
ORDER BY delegator, id;

-- name: RevokeDelegation :one
-- Revokes a delegation neither revoked nor ended at an instant.
UPDATE delegations SET revoked_at = sqlc.arg(at), revoked_by = sqlc.arg(revoked_by)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL AND ends_at > sqlc.arg(at)
RETURNING *;

-- name: ActiveDelegationsTo :many
-- The delegations active at an instant that a delegate holds.
SELECT * FROM delegations
WHERE delegate = sqlc.arg(delegate) AND revoked_at IS NULL AND starts_at <= sqlc.arg(at) AND ends_at > sqlc.arg(at)
ORDER BY delegator, id;

-- name: ShareDelegation :one
-- A delegation, locked until the transaction ends against its revocation: a review made on its
-- strength is recorded before the delegation is revoked, or not at all.
SELECT * FROM delegations WHERE id = $1 FOR SHARE;

-- name: RevokeDelegationsOf :many
-- Revokes the delegations neither revoked nor ended at an instant that a person gave or received:
-- they were deactivated.
UPDATE delegations SET revoked_at = sqlc.arg(at), revoked_by = sqlc.arg(revoked_by)
WHERE revoked_at IS NULL AND ends_at > sqlc.arg(at) AND (delegator = sqlc.arg(name) OR delegate = sqlc.arg(name))
RETURNING *;
