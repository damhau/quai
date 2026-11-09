-- name: GetUser :one
SELECT * FROM users WHERE name = $1;

-- name: RecordUserLogin :execrows
-- A login: the person as their ID token (or the development switcher) describes them now. A login
-- without the identity provider's identifiers (a development user) keeps the ones known. It changes
-- no row of a deactivated person, nor one another subject holds (open question 45): no row
-- affected, the login is refused, even when that changed after the caller looked.
INSERT INTO users (name, email, groups, subject, object_id, last_login, auth_time)
VALUES ($1, $2, $3, $4, $5, now(), sqlc.narg(auth_time))
ON CONFLICT (org_id, name) DO UPDATE SET
    email = EXCLUDED.email, groups = EXCLUDED.groups,
    subject = COALESCE(NULLIF(EXCLUDED.subject, ''), users.subject),
    object_id = COALESCE(NULLIF(EXCLUDED.object_id, ''), users.object_id),
    last_login = EXCLUDED.last_login, auth_time = EXCLUDED.auth_time, updated_at = now()
WHERE users.deactivated_at IS NULL
  AND (EXCLUDED.subject = '' OR users.subject = '' OR users.subject = EXCLUDED.subject);

-- name: ListUsers :many
SELECT * FROM users ORDER BY name COLLATE "C";
