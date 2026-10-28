-- name: ListRepositories :many
SELECT name, document, managed_by, generation FROM repositories ORDER BY name;

-- name: LockRepositories :exec
-- Serializes reconciliations of the repositories declared in configuration, between processes.
SELECT pg_advisory_xact_lock(hashtext('quai.repositories'));

-- name: PutConfigRepository :exec
-- Adds or updates a repository declared in configuration. The generation moves only when the
-- document changes; a repository managed through the API is left alone.
INSERT INTO repositories (name, document, managed_by) VALUES ($1, $2, 'config')
ON CONFLICT (org_id, name) DO UPDATE
SET document = EXCLUDED.document, generation = repositories.generation + 1, updated_at = now()
WHERE repositories.managed_by = 'config' AND repositories.document <> EXCLUDED.document;

-- name: DeleteConfigRepositoriesExcept :execrows
-- Removes the repositories declared in configuration that it no longer declares.
DELETE FROM repositories WHERE managed_by = 'config' AND NOT (name = ANY (sqlc.arg(keep)::text[]));

-- name: CreateRepository :exec
INSERT INTO repositories (name, document, managed_by) VALUES ($1, $2, 'api');

-- name: UpdateRepository :execrows
UPDATE repositories SET document = $2, generation = generation + 1, updated_at = now()
WHERE name = $1 AND managed_by = 'api';

-- name: DeleteRepository :execrows
DELETE FROM repositories WHERE name = $1 AND managed_by = 'api';
