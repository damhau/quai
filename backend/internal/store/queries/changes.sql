-- name: CreateChangeSet :one
INSERT INTO change_sets (id, request_id, repository, hash, fingerprints)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateChange :exec
INSERT INTO changes (change_set_id, path, op, before, after)
VALUES ($1, $2, $3, $4, $5);

-- name: ListChangeSets :many
SELECT * FROM change_sets WHERE request_id = $1 ORDER BY repository;

-- name: ListChanges :many
SELECT * FROM changes WHERE change_set_id = $1 ORDER BY path;

-- name: SetChangeSetHash :exec
UPDATE change_sets SET hash = $2 WHERE id = $1;

-- name: DeleteChanges :exec
DELETE FROM changes WHERE change_set_id = $1;

-- name: SetChangeSetCommit :exec
-- The commit that landed a change set: the direct commit, or the pull request's merge commit.
UPDATE change_sets SET commit = $3 WHERE request_id = $1 AND repository = $2;

-- name: RequestAtCommit :one
-- The request whose change set a commit landed; commit may be abbreviated.
SELECT request_id FROM change_sets
WHERE repository = sqlc.arg(repository) AND commit <> '' AND left(commit, length(sqlc.arg(commit)::text)) = sqlc.arg(commit)::text
ORDER BY created_at DESC
LIMIT 1;
