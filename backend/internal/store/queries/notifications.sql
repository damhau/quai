-- name: AddNotification :one
-- A recipient of an event on a channel, once: nothing when the row exists already (pgx.ErrNoRows).
-- created_at is the notifications' clock: a digest gathers what was added before it was due.
INSERT INTO notifications (event_id, recipient, channel, kind, state, last_error, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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
-- Nothing to send: before it is sent, or before its digest is.
UPDATE notifications SET state = 'skipped', last_error = $2
WHERE id = $1 AND state IN ('pending', 'digest');

-- name: WaitingForDigest :many
-- The recipients and channels with notifications that wait for a digest.
SELECT DISTINCT recipient, channel FROM notifications
WHERE state = 'digest' AND digest_id IS NULL
ORDER BY recipient, channel;

-- name: AddDigest :one
-- A recipient's digest on a channel for a period, once: nothing when it exists already
-- (pgx.ErrNoRows), so a job run twice makes one.
INSERT INTO notification_digests (recipient, channel, period, due, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (recipient, channel, period) DO NOTHING
RETURNING *;

-- name: GatherDigest :execrows
-- The notifications a digest gathers: those waiting for one, added before it was due. A
-- notification joins one digest only.
UPDATE notifications SET digest_id = sqlc.arg(digest_id)
WHERE recipient = sqlc.arg(recipient) AND channel = sqlc.arg(channel) AND state = 'digest'
  AND digest_id IS NULL AND created_at < sqlc.arg(before);

-- name: GetDigest :one
SELECT * FROM notification_digests WHERE id = $1;

-- name: ListDigestNotifications :many
-- The notifications of a digest still to tell.
SELECT * FROM notifications WHERE digest_id = $1 AND state = 'digest' ORDER BY id;

-- name: DigestSent :exec
UPDATE notification_digests SET state = 'sent', attempts = attempts + 1, last_error = '', sent_at = sqlc.arg(sent_at)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: DigestNotificationsSent :exec
UPDATE notifications SET state = 'sent', attempts = attempts + 1, last_error = '', sent_at = sqlc.arg(sent_at)
WHERE digest_id = sqlc.arg(digest_id) AND state = 'digest';

-- name: DigestAttempted :exec
-- An attempt that failed: state stays pending while attempts are left, failed after the last.
UPDATE notification_digests SET attempts = attempts + 1, last_error = $2, state = $3
WHERE id = $1 AND state = 'pending';

-- name: DigestNotificationsFailed :exec
UPDATE notifications SET state = 'failed', attempts = sqlc.arg(attempts), last_error = sqlc.arg(last_error)
WHERE digest_id = sqlc.arg(digest_id) AND state = 'digest';

-- name: DigestSkipped :exec
UPDATE notification_digests SET state = 'skipped', last_error = $2
WHERE id = $1 AND state = 'pending';
