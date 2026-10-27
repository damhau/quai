-- name: StartRun :one
INSERT INTO runs (id, repository, stack, commit, kind, status, url)
VALUES ($1, $2, $3, $4, $5, 'running', $6)
RETURNING *;

-- name: FinishRun :one
UPDATE runs SET status = $2, summary = $3, finished_at = now()
WHERE id = $1 AND status = 'running'
RETURNING *;

-- name: LatestRun :one
SELECT * FROM runs
WHERE repository = $1 AND stack = $2 AND kind = $3
ORDER BY started_at DESC, id
LIMIT 1;
