-- Repository connections (docs/design/01-repository-connections.md): each Repository document
-- as declared, by name. Those declared in configuration files are reconciled at start-up and
-- read-only through the API; the API manages the others. The generation moves whenever the
-- document changes, so that every process reloads what changed.

-- +goose Up
CREATE TABLE repositories (
    org_id     text NOT NULL DEFAULT 'default',
    name       text NOT NULL,
    document   jsonb NOT NULL,
    managed_by text NOT NULL CHECK (managed_by IN ('config', 'api')),
    generation bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, name)
);

-- +goose Down
DROP TABLE repositories;
