-- name: GetUser :one
SELECT * FROM users WHERE name = $1;

-- name: RecordUserLogin :execrows
-- A login: the person as their ID token (or the development switcher) describes them now. A login
-- without the identity provider's identifiers (a development user) keeps the ones known. The
-- manager is always the login's, none included: a lookup that failed leaves no manager, not the
-- one found before (open question 69). It changes no row of a deactivated person, nor one another
-- subject holds (open question 45): no row affected, the login is refused, even when that changed
-- after the caller looked.
INSERT INTO users (name, email, groups, subject, object_id, last_login, auth_time, manager_id, manager_source, manager_reason)
VALUES ($1, $2, $3, $4, $5, now(), sqlc.narg(auth_time), sqlc.arg(manager_id), sqlc.arg(manager_source), sqlc.arg(manager_reason))
ON CONFLICT (org_id, name) DO UPDATE SET
    email = EXCLUDED.email, groups = EXCLUDED.groups,
    subject = COALESCE(NULLIF(EXCLUDED.subject, ''), users.subject),
    object_id = COALESCE(NULLIF(EXCLUDED.object_id, ''), users.object_id),
    last_login = EXCLUDED.last_login, auth_time = EXCLUDED.auth_time,
    manager_id = EXCLUDED.manager_id, manager_source = EXCLUDED.manager_source, manager_reason = EXCLUDED.manager_reason,
    updated_at = now()
WHERE users.deactivated_at IS NULL
  AND (EXCLUDED.subject = '' OR users.subject = '' OR users.subject = EXCLUDED.subject);

-- name: UsersByIdentifier :many
-- The people the identity provider's immutable identifier names: by Entra ID oid (object_id, a
-- GUID, in any case), and by sub when by_subject. Two at most: more than one is already ambiguous.
SELECT * FROM users
WHERE sqlc.arg(id)::text <> ''
  AND (lower(object_id) = lower(sqlc.arg(id)::text) OR (sqlc.arg(by_subject)::boolean AND subject = sqlc.arg(id)::text))
ORDER BY name COLLATE "C"
LIMIT 2;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name COLLATE "C";
