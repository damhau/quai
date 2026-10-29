-- The run report protocol in full (docs/design/06-runs-status-and-drift.md#run-report-protocol):
-- the pull request a run is for, the request it belongs to, its policy results and cost, and
-- drift checks named drift-check. A change set records the commit that landed it (the direct
-- commit or the pull request's merge commit), so that runs at that commit find their request.

-- +goose Up
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
UPDATE runs SET kind = 'drift-check' WHERE kind = 'drift';
ALTER TABLE runs
    ADD CONSTRAINT runs_kind_check CHECK (kind IN ('plan', 'apply', 'sync', 'drift-check')),
    ADD COLUMN pull_request text NOT NULL DEFAULT '',
    ADD COLUMN request_id   uuid REFERENCES requests (id) ON DELETE SET NULL,
    ADD COLUMN policies     jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN cost         jsonb NOT NULL DEFAULT '{}';
CREATE INDEX runs_request ON runs (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX runs_running ON runs (org_id, repository, stack, kind) WHERE status = 'running';

ALTER TABLE change_sets ADD COLUMN commit text NOT NULL DEFAULT '';
CREATE INDEX change_sets_commit ON change_sets (repository, commit) WHERE commit <> '';

-- +goose Down
DROP INDEX change_sets_commit;
ALTER TABLE change_sets DROP COLUMN commit;
DROP INDEX runs_running;
DROP INDEX runs_request;
ALTER TABLE runs
    DROP CONSTRAINT runs_kind_check,
    DROP COLUMN cost,
    DROP COLUMN policies,
    DROP COLUMN request_id,
    DROP COLUMN pull_request;
UPDATE runs SET kind = 'drift' WHERE kind = 'drift-check';
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('plan', 'apply', 'sync', 'drift'));
