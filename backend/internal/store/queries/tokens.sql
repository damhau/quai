-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, owner, name, hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetActiveAPIToken :one
-- The token with this hash, if it is neither revoked nor expired.
SELECT * FROM api_tokens
WHERE hash = $1 AND revoked_at IS NULL AND expires_at > now();

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND owner = $2 AND revoked_at IS NULL;

-- name: ListAPITokens :many
SELECT * FROM api_tokens WHERE owner = $1 ORDER BY created_at, id;
