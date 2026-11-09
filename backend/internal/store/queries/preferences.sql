-- name: GetNotificationPreferences :one
SELECT * FROM notification_preferences WHERE name = $1;

-- name: SetNotificationPreferences :exec
-- A person's preferences: what differs from the defaults (NULL hour and time zone: the defaults').
INSERT INTO notification_preferences (name, modes, digest_hour, time_zone, updated_at)
VALUES (sqlc.arg(name), sqlc.arg(modes), sqlc.narg(digest_hour), sqlc.narg(time_zone), now())
ON CONFLICT (org_id, name) DO UPDATE SET modes = EXCLUDED.modes, digest_hour = EXCLUDED.digest_hour,
    time_zone = EXCLUDED.time_zone, updated_at = EXCLUDED.updated_at;
