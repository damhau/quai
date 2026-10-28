-- name: RecordWebhook :exec
INSERT INTO repository_sync (repository, last_webhook_at) VALUES ($1, now())
ON CONFLICT (org_id, repository) DO UPDATE SET last_webhook_at = now();

-- name: RecordFetch :exec
INSERT INTO repository_sync (repository, last_fetch_at, head) VALUES ($1, now(), $2)
ON CONFLICT (org_id, repository) DO UPDATE SET last_fetch_at = now(), head = EXCLUDED.head;

-- name: RecordPoll :exec
INSERT INTO repository_sync (repository, last_poll_at) VALUES ($1, now())
ON CONFLICT (org_id, repository) DO UPDATE SET last_poll_at = now();

-- name: GetRepositorySync :one
SELECT * FROM repository_sync WHERE repository = $1;

-- name: ListRepositorySync :many
SELECT * FROM repository_sync ORDER BY repository;

-- name: AddWebhookDelivery :execrows
INSERT INTO webhook_deliveries (repository, delivery_id) VALUES ($1, $2)
ON CONFLICT (org_id, repository, delivery_id) DO NOTHING;

-- name: DeleteWebhookDeliveriesBefore :execrows
DELETE FROM webhook_deliveries WHERE received_at < $1;

-- name: RecordTrust :exec
INSERT INTO repository_sync (repository, trust_problems, trust_checked_at) VALUES ($1, $2, now())
ON CONFLICT (org_id, repository) DO UPDATE SET trust_problems = EXCLUDED.trust_problems, trust_checked_at = now();
