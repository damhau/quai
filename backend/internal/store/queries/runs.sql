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

-- name: RunningRun :one
-- The running run a report continues (docs/design/06-runs-status-and-drift.md#run-report-protocol):
-- same repository, stack and kind, and the same url, or, when either has none, the same commit
-- and pull request.
SELECT * FROM runs
WHERE repository = sqlc.arg(repository) AND stack = sqlc.arg(stack) AND kind = sqlc.arg(kind) AND status = 'running'
  AND ((sqlc.arg(url)::text <> '' AND url = sqlc.arg(url)::text)
    OR ((url = '' OR sqlc.arg(url)::text = '') AND commit = sqlc.arg(commit) AND pull_request = sqlc.arg(pull_request)))
ORDER BY started_at DESC, id
LIMIT 1
FOR UPDATE;

-- name: InsertRun :one
INSERT INTO runs (id, repository, stack, commit, kind, status, url, pull_request, request_id, summary, policies, cost,
                  started_at, finished_at)
VALUES (sqlc.arg(id), sqlc.arg(repository), sqlc.arg(stack), sqlc.arg(commit), sqlc.arg(kind), sqlc.arg(status), sqlc.arg(url),
        sqlc.arg(pull_request), sqlc.arg(request_id), sqlc.arg(summary), sqlc.arg(policies), sqlc.arg(cost),
        COALESCE(sqlc.narg(started_at)::timestamptz, now()),
        CASE WHEN sqlc.arg(status) = 'running' THEN NULL ELSE COALESCE(sqlc.narg(finished_at)::timestamptz, now()) END)
RETURNING *;

-- name: UpdateRun :one
-- A later report of a running run: what it says replaces what the run had, and what it leaves
-- out is kept.
UPDATE runs SET
    status       = sqlc.arg(status),
    url          = CASE WHEN sqlc.arg(url)::text <> '' THEN sqlc.arg(url)::text ELSE url END,
    request_id   = COALESCE(request_id, sqlc.narg(request_id)::uuid),
    summary      = CASE WHEN sqlc.arg(summary)::jsonb <> '{}' THEN sqlc.arg(summary)::jsonb ELSE summary END,
    policies     = CASE WHEN sqlc.arg(policies)::jsonb <> '[]' THEN sqlc.arg(policies)::jsonb ELSE policies END,
    cost         = CASE WHEN sqlc.arg(cost)::jsonb <> '{}' THEN sqlc.arg(cost)::jsonb ELSE cost END,
    started_at   = COALESCE(sqlc.narg(started_at)::timestamptz, started_at),
    finished_at  = CASE WHEN sqlc.arg(status) = 'running' THEN NULL ELSE COALESCE(sqlc.narg(finished_at)::timestamptz, now()) END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ListRuns :many
-- A repository's runs, newest first; stack and kind narrow them when given.
SELECT * FROM runs
WHERE repository = sqlc.arg(repository)
  AND (sqlc.arg(stack)::text = '' OR stack = sqlc.arg(stack)::text)
  AND (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
ORDER BY started_at DESC, id
LIMIT sqlc.arg(max_rows);
