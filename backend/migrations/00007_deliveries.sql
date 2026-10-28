-- Deliveries (docs/design/05-delivery-and-approvals.md#pull-request-protocol): how a request's
-- change reaches its repository in pull-request mode: its branch, its pull request on the
-- platform, the head Quai pushed and, once merged, the merge commit.

-- +goose Up
CREATE TABLE deliveries (
    request_id   uuid PRIMARY KEY REFERENCES requests (id) ON DELETE CASCADE,
    org_id       text NOT NULL DEFAULT 'default',
    repository   text NOT NULL,
    branch       text NOT NULL,
    pull_request text NOT NULL DEFAULT '',
    url          text NOT NULL DEFAULT '',
    head         text NOT NULL DEFAULT '',
    merge_commit text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX deliveries_pull_request ON deliveries (org_id, repository, pull_request) WHERE pull_request <> '';

-- +goose Down
DROP TABLE deliveries;
