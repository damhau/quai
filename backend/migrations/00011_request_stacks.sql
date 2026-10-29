-- The stacks a request's change set is in (docs/design/06-runs-status-and-drift.md#runs-and-requests):
-- with a runner, a merged request is applying until an apply of each of them includes the
-- commit that landed it.

-- +goose Up
ALTER TABLE requests ADD COLUMN stacks text[] NOT NULL DEFAULT '{}';
UPDATE requests SET stacks = ARRAY[stack] WHERE stack <> '';
CREATE INDEX requests_applying ON requests (org_id) WHERE state = 'applying';

-- +goose Down
DROP INDEX requests_applying;
ALTER TABLE requests DROP COLUMN stacks;
