-- Enforcement (docs/design/01-repository-connections.md#health): whether the target branch
-- requires quai/approval as Quai needs it, as last verified: what is missing, if anything.
-- Verified at registration, hourly and when a branch policy changes.

-- +goose Up
ALTER TABLE repository_sync
    ADD COLUMN enforcement_problems   jsonb,
    ADD COLUMN enforcement_checked_at timestamptz;

-- +goose Down
ALTER TABLE repository_sync
    DROP COLUMN enforcement_checked_at,
    DROP COLUMN enforcement_problems;
