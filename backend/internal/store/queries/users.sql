-- name: GetUser :one
SELECT * FROM users WHERE name = $1;

-- name: RecordUserLogin :one
-- A login: the person as their ID token (or the development switcher) describes them now. A login
-- without the identity provider's identifiers (a development user) keeps the ones known. The
-- manager is always the login's, none included: a lookup that failed leaves no manager, not the
-- one found before (open question 69); a person SCIM provisions keeps SCIM's (open question 87).
-- Their groups are the login's, less those of the groups SCIM provisioned that do not list them
-- (open question 85). It changes no row of a deactivated person, nor one another subject holds
-- (open question 45), nor one SCIM bound to another identifier than binding (the login's oid, or
-- its sub): no row, the login is refused, even when that changed after the caller looked. It
-- returns what the session keeps: the groups and the epoch.
INSERT INTO users (name, email, groups, login_groups, subject, object_id, last_login, auth_time, manager_id, manager_source, manager_reason)
VALUES (sqlc.arg(name), sqlc.arg(email), sqlc.arg(groups)::text[], sqlc.arg(groups)::text[], sqlc.arg(subject), sqlc.arg(object_id), now(),
        sqlc.narg(auth_time), sqlc.arg(manager_id), sqlc.arg(manager_source), sqlc.arg(manager_reason))
ON CONFLICT (org_id, name) DO UPDATE SET
    email = EXCLUDED.email, login_groups = EXCLUDED.login_groups,
    groups = CASE WHEN users.scim_id IS NULL THEN EXCLUDED.groups ELSE ARRAY(
        SELECT t.g FROM unnest(sqlc.arg(groups)::text[]) WITH ORDINALITY AS t(g, i)
        WHERE NOT EXISTS (SELECT 1 FROM scim_known_groups k WHERE k.org_id = users.org_id AND k.kind = sqlc.arg(group_kind)::text AND k.name = t.g)
           OR EXISTS (SELECT 1 FROM scim_members m JOIN scim_groups sg ON sg.id = m.group_id
                      WHERE m.org_id = users.org_id AND m.user_name = users.name
                        AND CASE WHEN sqlc.arg(group_kind)::text = 'displayName' THEN sg.display_name ELSE sg.external_id END = t.g)
        ORDER BY t.i) END,
    subject = COALESCE(NULLIF(EXCLUDED.subject, ''), users.subject),
    object_id = COALESCE(NULLIF(EXCLUDED.object_id, ''), users.object_id),
    last_login = EXCLUDED.last_login, auth_time = EXCLUDED.auth_time,
    manager_id = CASE WHEN users.scim_id IS NULL THEN EXCLUDED.manager_id ELSE users.manager_id END,
    manager_source = CASE WHEN users.scim_id IS NULL THEN EXCLUDED.manager_source ELSE users.manager_source END,
    manager_reason = CASE WHEN users.scim_id IS NULL THEN EXCLUDED.manager_reason ELSE users.manager_reason END,
    updated_at = now()
WHERE users.deactivated_at IS NULL
  AND (EXCLUDED.subject = '' OR users.subject = '' OR users.subject = EXCLUDED.subject)
  AND (users.scim_id IS NULL OR (sqlc.arg(binding)::text <> '' AND lower(users.scim_external_id) = lower(sqlc.arg(binding)::text)))
RETURNING groups, session_epoch;

-- name: UserByBinding :one
-- The person SCIM provisions with this immutable identifier of the identity provider, in any
-- case: the one a sign-in with it is, whatever its name claim says.
SELECT * FROM users WHERE scim_id IS NOT NULL AND lower(scim_external_id) = lower(sqlc.arg(binding)::text);

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

-- name: UsersByEmail :many
-- The people whose email, which the directory keeps only verified, is one of emails, in any case.
SELECT * FROM users
WHERE email <> '' AND lower(email) = ANY(COALESCE(sqlc.narg(emails)::text[], '{}'))
ORDER BY name COLLATE "C";
