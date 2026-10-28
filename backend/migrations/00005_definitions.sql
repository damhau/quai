-- Definitions per commit (docs/design/01-repository-connections.md#definitions): for each commit
-- of a repository Quai loaded, the definitions active there, by kind and name, with the digest
-- of their files and the commit their version comes from (an earlier one when the definition is
-- broken at this commit, so a broken edit never takes a catalog item down), and the problems.
-- Plans keep the commit; a definition whose digest changed since makes the request stale.

-- +goose Up
CREATE TABLE definition_sets (
    org_id     text NOT NULL DEFAULT 'default',
    repository text NOT NULL,
    commit     text NOT NULL,
    active     jsonb NOT NULL,
    problems   jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, repository, commit)
);

CREATE INDEX definition_sets_latest ON definition_sets (org_id, repository, created_at DESC);

-- +goose Down
DROP TABLE definition_sets;
