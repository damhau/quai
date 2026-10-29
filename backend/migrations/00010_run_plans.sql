-- What a run's plan changes, by Quai resource (docs/design/06-runs-status-and-drift.md#run-report-protocol):
-- addresses, actions and changed attribute names, and the values of non-sensitive attributes
-- when the repository opts in. Never the plan itself.

-- +goose Up
ALTER TABLE runs ADD COLUMN plan jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE runs DROP COLUMN plan;
