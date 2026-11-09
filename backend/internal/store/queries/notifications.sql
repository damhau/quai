-- name: AddNotification :one
-- A recipient of an event on a channel, once: nothing when the row exists already (pgx.ErrNoRows).
INSERT INTO notifications (event_id, recipient, channel, kind, state, last_error)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (event_id, recipient, channel) DO NOTHING
RETURNING *;

-- name: GetNotification :one
SELECT * FROM notifications WHERE id = $1;

-- name: ListEventNotifications :many
SELECT * FROM notifications WHERE event_id = $1 ORDER BY id;

-- name: NotificationSent :exec
UPDATE notifications SET state = 'sent', attempts = attempts + 1, last_error = '', sent_at = now()
WHERE id = $1 AND state = 'pending';

-- name: NotificationAttempted :exec
-- An attempt that failed: state stays pending while attempts are left, failed after the last.
UPDATE notifications SET attempts = attempts + 1, last_error = $2, state = $3
WHERE id = $1 AND state = 'pending';

-- name: NotificationSkipped :exec
UPDATE notifications SET state = 'skipped', last_error = $2
WHERE id = $1 AND state = 'pending';
