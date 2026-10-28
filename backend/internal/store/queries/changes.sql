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
