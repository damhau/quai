-- SCIM provisioning (docs/design/08-identity-access-tenancy.md#scim-provisioning, P4-15).
--
-- A person the identity provider provisions is a row of users bound to it: scim_id is the id Quai
-- gave the SCIM resource, scim_external_id the provider's immutable identifier of the person
-- (Entra ID's object ID, the oid of their ID tokens), scim_user_name the userName it sends, which
-- may change while the person's name in Quai never does. A DELETE unbinds the row (scim_id NULL)
-- and leaves it deactivated, its name and its history kept: the name is never given to someone
-- else (open question 84).
--
-- login_groups are the groups of the person's last sign-in; groups are what they hold: the same,
-- except for a provisioned person, who loses those of a group SCIM has provisioned that does not
-- list them (open question 85). SCIM never gives a group.
--
-- session_epoch counts the person's deactivations: a session remembers the epoch it signed in
-- at, and one from before the last deactivation is over, a reactivation notwithstanding.
--
-- scim_groups and scim_members are the groups SCIM provisions and their members, people by name.
-- scim_known_groups keeps every name a provisioned group held, by kind (externalId, displayName),
-- also after the group is renamed or deleted: SCIM decides who holds such a name.
--
-- scim_tokens records when each SCIM token (auth.scim.tokens, by name) was last used; the tokens
-- themselves are in the configuration's secrets, as their SHA-256 (open question 83).

-- +goose Up
ALTER TABLE users
    ADD COLUMN scim_id          uuid UNIQUE,
    ADD COLUMN scim_external_id text NOT NULL DEFAULT '',
    ADD COLUMN scim_user_name   text NOT NULL DEFAULT '',
    ADD COLUMN login_groups     text[] NOT NULL DEFAULT '{}',
    ADD COLUMN session_epoch    bigint NOT NULL DEFAULT 0;
UPDATE users SET login_groups = groups;
CREATE UNIQUE INDEX users_scim_external_id ON users (org_id, lower(scim_external_id)) WHERE scim_id IS NOT NULL;
CREATE UNIQUE INDEX users_scim_user_name ON users (org_id, lower(scim_user_name)) WHERE scim_id IS NOT NULL;

CREATE TABLE scim_groups (
    id           uuid PRIMARY KEY,
    org_id       text NOT NULL DEFAULT 'default',
    external_id  text NOT NULL DEFAULT '',
    display_name text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX scim_groups_display_name ON scim_groups (org_id, lower(display_name));
CREATE UNIQUE INDEX scim_groups_external_id ON scim_groups (org_id, external_id) WHERE external_id <> '';

CREATE TABLE scim_members (
    org_id    text NOT NULL DEFAULT 'default',
    group_id  uuid NOT NULL REFERENCES scim_groups (id) ON DELETE CASCADE,
    user_name text NOT NULL,
    PRIMARY KEY (group_id, user_name),
    FOREIGN KEY (org_id, user_name) REFERENCES users (org_id, name) ON DELETE CASCADE
);
CREATE INDEX scim_members_user ON scim_members (org_id, user_name);

CREATE TABLE scim_known_groups (
    org_id text NOT NULL DEFAULT 'default',
    kind   text NOT NULL CHECK (kind IN ('externalId', 'displayName')),
    name   text NOT NULL,
    PRIMARY KEY (org_id, kind, name)
);

CREATE TABLE scim_tokens (
    name         text PRIMARY KEY,
    last_used_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE scim_tokens;
DROP TABLE scim_known_groups;
DROP TABLE scim_members;
DROP TABLE scim_groups;
DROP INDEX users_scim_user_name;
DROP INDEX users_scim_external_id;
ALTER TABLE users
    DROP COLUMN session_epoch, DROP COLUMN login_groups, DROP COLUMN scim_user_name,
    DROP COLUMN scim_external_id, DROP COLUMN scim_id;
