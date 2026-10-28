-- Trust (docs/design/01-repository-connections.md#trust): whether a repository's definitions can
-- only change through review, as the last poll found it: what is missing, if anything.

-- +goose Up
ALTER TABLE repository_sync
    ADD COLUMN trust_problems   jsonb,
    ADD COLUMN trust_checked_at timestamptz;

-- +goose Down
ALTER TABLE repository_sync
    DROP COLUMN trust_checked_at,
    DROP COLUMN trust_problems;
