-- ServiceNow change requests (docs/design/05-delivery-and-approvals.md#itsm, P4-19).
--
-- change_requests is the change request Quai creates in ServiceNow for a request an
-- external: servicenow rule applies to, one per content: change_set_hash is the change set it is
-- for. When the request's content changes, its change request is superseded (superseded_at) and a
-- new one is made for the new content, the next generation; an approval never counts for other
-- content (open question 107). At most one is current (not superseded) per request.
--
-- Quai trusts only the change request it created: correlation_id, quai:<request>:<generation>:<random>,
-- is set on it so that a creation tried again finds the one already made, and no one can make one
-- before Quai that it would take for its own; sys_id, once ServiceNow answered it, is the only
-- record Quai reads the state of. number is for people.
--
-- Opening a change request takes steps that each survive a crash: created (opened_at, with its
-- sys_id), then its long fields written and taken for approval (prepared_at). state, state_label
-- and approval are what ServiceNow last reported, outcome what they mean
-- (servicenow.Record.Outcome): pending until an explicit approval, approved, rejected, cancelled or
-- closed. Quai opens or reads it at next_poll_at (NULL: now); failures, failure and failing_since
-- are the calls that failed since the last that worked, shown on the request page while ServiceNow
-- is unreachable; problem is what ServiceNow refused that polling does not wait for.

-- +goose Up
CREATE TABLE change_requests (
    id              uuid PRIMARY KEY,
    request_id      uuid NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    org_id          text NOT NULL DEFAULT 'default',
    generation      integer NOT NULL CHECK (generation > 0),
    correlation_id  text NOT NULL UNIQUE,
    change_set_hash text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('normal', 'standard')),
    template        text NOT NULL DEFAULT '',
    sys_id          text UNIQUE,
    number          text NOT NULL DEFAULT '',
    state           text NOT NULL DEFAULT '',
    state_label     text NOT NULL DEFAULT '',
    approval        text NOT NULL DEFAULT '',
    outcome         text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'approved', 'rejected', 'cancelled', 'closed')),
    created_at      timestamptz NOT NULL,
    opened_at       timestamptz,
    prepared_at     timestamptz,
    polled_at       timestamptz,
    next_poll_at    timestamptz,
    failures        integer NOT NULL DEFAULT 0,
    failure         text NOT NULL DEFAULT '',
    failing_since   timestamptz,
    problem         text NOT NULL DEFAULT '',
    superseded_at   timestamptz,
    UNIQUE (request_id, generation)
);

CREATE UNIQUE INDEX change_requests_current ON change_requests (request_id) WHERE superseded_at IS NULL;
CREATE INDEX change_requests_due ON change_requests (next_poll_at) WHERE superseded_at IS NULL;

-- +goose Down
DROP TABLE change_requests;
