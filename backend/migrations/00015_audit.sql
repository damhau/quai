-- The audit log (docs/design/09-security-and-compliance.md#audit-log): the security-relevant
-- events of the installation, one chain for all of it, apart from the events timeline. Each event
-- is chained on the one before by its hash, hash = SHA-256(prev || RFC 8785 JSON of the event
-- without prev and hash) (open question 18), and written in the transaction of what it records.
-- Personal data is in the event as "pd:" and the SHA-256 of a random salt and the value; the salt
-- and the value are in audit_personal, which pseudonymization deletes from without breaking the
-- chain.
--
-- The application role reads and appends, nothing else: quai migrate, connected as the schema
-- owner, grants it SELECT and INSERT on these tables and no UPDATE, DELETE or TRUNCATE (open
-- question 19). Pruning, later, goes through a SECURITY DEFINER function that this owner creates.
--
-- The reviewer's authentication time is kept with the review, for the evidence bundle.

-- +goose Up
CREATE TABLE audit_events (
    seq     bigint PRIMARY KEY CHECK (seq > 0),
    at      timestamptz NOT NULL,
    actor   jsonb NOT NULL,
    action  text NOT NULL,
    object  jsonb NOT NULL,
    details jsonb NOT NULL,
    prev    text NOT NULL,
    hash    text NOT NULL
);
CREATE INDEX audit_events_action ON audit_events (action, seq);
CREATE INDEX audit_events_actor ON audit_events ((actor ->> 'id'), seq);
CREATE INDEX audit_events_object ON audit_events ((object ->> 'id'), seq);
CREATE INDEX audit_events_at ON audit_events (at);

-- One row per personal value of an event: deleting it leaves the event's hash unexplained.
CREATE TABLE audit_personal (
    hash  text PRIMARY KEY,
    seq   bigint NOT NULL,
    salt  bytea NOT NULL,
    value text NOT NULL
);
CREATE INDEX audit_personal_seq ON audit_personal (seq);
-- A hash index: values (comments, reasons) can be longer than a B-tree entry.
CREATE INDEX audit_personal_value ON audit_personal USING hash (value);

ALTER TABLE reviews ADD COLUMN reviewer_auth_time timestamptz;

-- +goose Down
ALTER TABLE reviews DROP COLUMN reviewer_auth_time;
DROP TABLE audit_personal;
DROP TABLE audit_events;
