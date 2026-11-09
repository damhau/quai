-- The directory (docs/design/08-identity-access-tenancy.md#directory): each person who signs in,
-- as their last login described them, which the access report lists; from P4-15, each person SCIM
-- provisions too. A person is their name, as everywhere in Quai (requesters, reviewers, owners).
--
-- Personal data: names, emails, groups and the identity provider's identifiers. Only an email the
-- identity provider vouches for is kept (open question 30). Nothing deletes a row yet (open
-- question 44).
--
-- A review keeps the reviewer's teams when they reviewed, as it keeps their groups: team: owner
-- entries match them. Reviews from before have none, and their groups were their teams then.

-- +goose Up
CREATE TABLE users (
    org_id         text NOT NULL DEFAULT 'default',
    name           text NOT NULL,
    email          text NOT NULL DEFAULT '',
    groups         text[] NOT NULL DEFAULT '{}',
    -- The ID token's sub, and Entra ID's oid; empty for development users.
    subject        text NOT NULL DEFAULT '',
    object_id      text NOT NULL DEFAULT '',
    last_login     timestamptz,
    auth_time      timestamptz,
    deactivated_at timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, name)
);

ALTER TABLE reviews ADD COLUMN teams text[];

-- +goose Down
ALTER TABLE reviews DROP COLUMN teams;
DROP TABLE users;
