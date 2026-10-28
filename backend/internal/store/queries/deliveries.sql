-- name: PutDelivery :one
INSERT INTO deliveries (request_id, repository, branch, pull_request, url, head) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (request_id) DO UPDATE SET pull_request = EXCLUDED.pull_request, url = EXCLUDED.url, head = EXCLUDED.head, updated_at = now()
RETURNING *;

-- name: GetDelivery :one
SELECT * FROM deliveries WHERE request_id = $1;

-- name: GetDeliveryByPullRequest :one
SELECT * FROM deliveries WHERE repository = $1 AND pull_request = $2;

-- name: SetDeliveryMerged :exec
UPDATE deliveries SET merge_commit = $2, updated_at = now() WHERE request_id = $1;

-- name: ListAwaitedPullRequests :many
-- The pull requests of a repository whose requests are not over: Quai waits on their merge.
SELECT d.pull_request FROM deliveries d JOIN requests r ON r.id = d.request_id
WHERE d.repository = $1 AND d.pull_request <> '' AND r.state IN ('pending_approval', 'approved', 'merging', 'stale');

-- name: ListOpenDeliveries :many
-- The pull requests of a repository whose requests still wait on approval or merge.
SELECT d.* FROM deliveries d JOIN requests r ON r.id = d.request_id
WHERE d.repository = $1 AND d.pull_request <> '' AND r.state IN ('pending_approval', 'approved', 'merging')
ORDER BY d.created_at;

-- name: PullRequestHealth :one
-- A repository's pull requests waiting on Quai, the oldest one's opening, and the stale ones.
SELECT count(*) FILTER (WHERE r.state IN ('pending_approval', 'approved', 'merging'))::bigint AS open,
       min(d.created_at) FILTER (WHERE r.state IN ('pending_approval', 'approved', 'merging'))::timestamptz AS oldest,
       count(*) FILTER (WHERE r.state = 'stale')::bigint AS stale
FROM deliveries d JOIN requests r ON r.id = d.request_id
WHERE d.repository = $1 AND d.pull_request <> '';
