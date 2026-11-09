-- How far the export of the audit log to each SIEM got
-- (docs/design/09-security-and-compliance.md#export-to-a-siem).
--
-- The worker sends the events after a destination's cursor, in seq order, and moves the cursor
-- to the last one the destination took: at least once, since a worker stopped between the two
-- sends them again. A destination is named by the configuration (audit.export[].name); renaming
-- it starts its export again from the first event kept.
--
-- This is not an audit table: the application role writes it, as it writes the events, so a
-- compromised server could move a cursor ahead and skip events. Each record carries its seq and
-- hash, so the SIEM sees the gap, and the chain still holds every event.

-- +goose Up
CREATE TABLE audit_export_cursors (
    destination text PRIMARY KEY,
    seq         bigint NOT NULL CHECK (seq > 0),
    hash        text NOT NULL,
    exported    timestamptz NOT NULL
);

-- +goose Down
DROP TABLE audit_export_cursors;
