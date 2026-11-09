-- SCIM provisioning (migrations/00028_scim.sql, internal/scim). A filter's text never reaches
-- SQL: internal/scim parses it into these parameters.

-- name: SCIMUser :one
SELECT * FROM users WHERE scim_id = $1;

-- name: LockSCIMUser :one
SELECT * FROM users WHERE scim_id = $1 FOR UPDATE;

-- name: LockUser :one
SELECT * FROM users WHERE name = $1 FOR UPDATE;

-- name: SCIMUserByUserName :one
-- The provisioned person with this userName, in any case (userName is not case-exact).
SELECT * FROM users WHERE scim_id IS NOT NULL AND lower(scim_user_name) = lower(sqlc.arg(user_name)::text);

-- name: UserNamesFolded :many
-- The names of the directory equal to name in any case.
SELECT name FROM users WHERE lower(name) = lower(sqlc.arg(name)::text) ORDER BY name COLLATE "C";

-- name: SCIMUsersByID :many
-- The provisioned people with these ids, locked: none is deleted while they join a group.
SELECT * FROM users WHERE scim_id = ANY (sqlc.arg(ids)::uuid[]) ORDER BY name COLLATE "C" FOR UPDATE;

-- name: ListSCIMUsers :many
-- The provisioned people a filter selects, by name, a page of them.
SELECT * FROM users
WHERE scim_id IS NOT NULL
  AND (NOT sqlc.arg(by_user_name)::boolean OR lower(scim_user_name) = lower(sqlc.arg(user_name)::text))
  AND (NOT sqlc.arg(by_external_id)::boolean OR scim_external_id = sqlc.arg(external_id)::text)
  AND (NOT sqlc.arg(by_id)::boolean OR scim_id = sqlc.arg(id)::uuid)
  AND (NOT sqlc.arg(by_manager)::boolean OR (manager_source = 'scim' AND manager_id = sqlc.arg(manager)::text))
ORDER BY name COLLATE "C"
LIMIT sqlc.arg(take) OFFSET sqlc.arg(skip);

-- name: CountSCIMUsers :one
SELECT count(*) FROM users
WHERE scim_id IS NOT NULL
  AND (NOT sqlc.arg(by_user_name)::boolean OR lower(scim_user_name) = lower(sqlc.arg(user_name)::text))
  AND (NOT sqlc.arg(by_external_id)::boolean OR scim_external_id = sqlc.arg(external_id)::text)
  AND (NOT sqlc.arg(by_id)::boolean OR scim_id = sqlc.arg(id)::uuid)
  AND (NOT sqlc.arg(by_manager)::boolean OR (manager_source = 'scim' AND manager_id = sqlc.arg(manager)::text));

-- name: CreateSCIMUser :one
-- A person SCIM provisions, new to the directory: their manager comes from SCIM alone.
INSERT INTO users (name, scim_id, scim_external_id, scim_user_name, deactivated_at, manager_id, manager_source, manager_reason)
VALUES (sqlc.arg(name), sqlc.arg(scim_id), sqlc.arg(external_id), sqlc.arg(user_name), sqlc.narg(deactivated_at),
        sqlc.arg(manager_id), 'scim', sqlc.arg(manager_reason))
RETURNING *;

-- name: AdoptSCIMUser :one
-- Binds a person of the directory, who signed in before, to SCIM.
UPDATE users SET scim_id = sqlc.arg(scim_id), scim_external_id = sqlc.arg(external_id), scim_user_name = sqlc.arg(user_name),
    manager_id = sqlc.arg(manager_id), manager_source = 'scim', manager_reason = sqlc.arg(manager_reason), updated_at = now()
WHERE name = sqlc.arg(name) AND scim_id IS NULL
RETURNING *;

-- name: UpdateSCIMUser :one
UPDATE users SET scim_user_name = sqlc.arg(user_name), scim_external_id = sqlc.arg(external_id),
    manager_id = sqlc.arg(manager_id), manager_source = 'scim', manager_reason = sqlc.arg(manager_reason), updated_at = now()
WHERE scim_id = sqlc.arg(scim_id)
RETURNING *;

-- name: DeactivateSCIMUser :one
-- Deactivates a provisioned person: their sessions end with the epoch.
UPDATE users SET deactivated_at = now(), session_epoch = session_epoch + 1, updated_at = now()
WHERE scim_id = $1 AND deactivated_at IS NULL
RETURNING *;

-- name: ReactivateSCIMUser :one
-- Reactivates a provisioned person: nothing their deactivation ended comes back.
UPDATE users SET deactivated_at = NULL, updated_at = now()
WHERE scim_id = $1 AND deactivated_at IS NOT NULL
RETURNING *;

-- name: UnbindSCIMUser :exec
-- A SCIM DELETE: the row stays, deactivated, and keeps its name and external ID.
UPDATE users SET scim_id = NULL, updated_at = now() WHERE scim_id = $1;

-- name: RefreshSCIMGroups :many
-- Works out again the groups of the provisioned people named: their last sign-in's, less those of
-- a group SCIM provisioned (scim_known_groups, by kind) that does not list them. It returns each
-- one's groups before and after.
WITH prior AS (
    SELECT org_id, name, groups FROM users
    WHERE scim_id IS NOT NULL AND name = ANY (sqlc.arg(names)::text[])
    ORDER BY name FOR UPDATE
)
UPDATE users u SET groups = ARRAY(
        SELECT t.g FROM unnest(u.login_groups) WITH ORDINALITY AS t(g, i)
        WHERE NOT EXISTS (SELECT 1 FROM scim_known_groups k WHERE k.org_id = u.org_id AND k.kind = sqlc.arg(group_kind)::text AND k.name = t.g)
           OR EXISTS (SELECT 1 FROM scim_members m JOIN scim_groups sg ON sg.id = m.group_id
                      WHERE m.org_id = u.org_id AND m.user_name = u.name
                        AND CASE WHEN sqlc.arg(group_kind)::text = 'displayName' THEN sg.display_name ELSE sg.external_id END = t.g)
        ORDER BY t.i),
    updated_at = now()
FROM prior p
WHERE u.org_id = p.org_id AND u.name = p.name
RETURNING u.name, p.groups AS groups_before, u.groups AS groups_after;

-- name: SCIMGroup :one
SELECT * FROM scim_groups WHERE id = $1;

-- name: LockSCIMGroup :one
SELECT * FROM scim_groups WHERE id = $1 FOR UPDATE;

-- name: ListSCIMGroups :many
SELECT * FROM scim_groups g
WHERE (NOT sqlc.arg(by_display_name)::boolean OR lower(g.display_name) = lower(sqlc.arg(display_name)::text))
  AND (NOT sqlc.arg(by_external_id)::boolean OR g.external_id = sqlc.arg(external_id)::text)
  AND (NOT sqlc.arg(by_id)::boolean OR g.id = sqlc.arg(id)::uuid)
  AND (NOT sqlc.arg(by_member)::boolean OR EXISTS (
      SELECT 1 FROM scim_members m JOIN users u ON u.org_id = m.org_id AND u.name = m.user_name
      WHERE m.group_id = g.id AND u.scim_id = sqlc.arg(member)::uuid))
ORDER BY lower(g.display_name), g.id
LIMIT sqlc.arg(take) OFFSET sqlc.arg(skip);

-- name: CountSCIMGroups :one
SELECT count(*) FROM scim_groups g
WHERE (NOT sqlc.arg(by_display_name)::boolean OR lower(g.display_name) = lower(sqlc.arg(display_name)::text))
  AND (NOT sqlc.arg(by_external_id)::boolean OR g.external_id = sqlc.arg(external_id)::text)
  AND (NOT sqlc.arg(by_id)::boolean OR g.id = sqlc.arg(id)::uuid)
  AND (NOT sqlc.arg(by_member)::boolean OR EXISTS (
      SELECT 1 FROM scim_members m JOIN users u ON u.org_id = m.org_id AND u.name = m.user_name
      WHERE m.group_id = g.id AND u.scim_id = sqlc.arg(member)::uuid));

-- name: CreateSCIMGroup :one
INSERT INTO scim_groups (id, external_id, display_name) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateSCIMGroup :one
UPDATE scim_groups SET external_id = $2, display_name = $3, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteSCIMGroup :execrows
DELETE FROM scim_groups WHERE id = $1;

-- name: AddKnownSCIMGroup :exec
-- A name a provisioned group held: from now on, SCIM decides who holds it.
INSERT INTO scim_known_groups (kind, name) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: SCIMGroupMembers :many
-- The members of groups: provisioned people.
SELECT m.group_id, m.user_name, u.scim_id, u.scim_user_name FROM scim_members m
JOIN users u ON u.org_id = m.org_id AND u.name = m.user_name
WHERE m.group_id = ANY (sqlc.arg(groups)::uuid[]) AND u.scim_id IS NOT NULL
ORDER BY m.group_id, m.user_name COLLATE "C";

-- name: SCIMMemberNames :many
SELECT user_name FROM scim_members WHERE group_id = $1 ORDER BY user_name COLLATE "C";

-- name: AddSCIMMember :execrows
INSERT INTO scim_members (group_id, user_name) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: RemoveSCIMMember :execrows
DELETE FROM scim_members WHERE group_id = $1 AND user_name = $2;

-- name: RemoveSCIMMemberships :many
-- A person's memberships, removed; the groups they were in.
DELETE FROM scim_members WHERE user_name = $1 RETURNING group_id;

-- name: TouchSCIMToken :exec
INSERT INTO scim_tokens (name, last_used_at) VALUES ($1, now())
ON CONFLICT (name) DO UPDATE SET last_used_at = now();

-- name: ListSCIMTokens :many
SELECT * FROM scim_tokens ORDER BY name;

-- name: LockSCIM :exec
-- Serializes SCIM's changes, between processes: they lock people and groups in no fixed order.
SELECT pg_advisory_xact_lock(hashtext('quai.scim'));

-- name: ProvisionedHolding :many
-- The provisioned people whose last sign-in gave them one of groups.
SELECT name FROM users WHERE scim_id IS NOT NULL AND login_groups && sqlc.arg(groups)::text[] ORDER BY name COLLATE "C";
