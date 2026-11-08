-- Signed checkpoints and retention of the audit log
-- (docs/design/09-security-and-compliance.md#audit-log).
--
-- Every hour and at shutdown, the worker signs the latest event's seq and hash with an Ed25519
-- key from the secret store: a checkpoint. The application role appends checkpoints and reads
-- them, nothing else (quai migrate grants it, as for the events).
--
-- The retention is the schema owner's: quai migrate sets it from audit.retention, and the
-- application role may only read it (open question 36). Pruning goes through audit_prune(), a
-- SECURITY DEFINER function of the owner, which the application role may execute and which
-- deletes only events older than the retention, by the database's clock, before a checkpoint
-- whose hash is its event's (open question 19). That checkpoint and its event are kept, so the
-- chain verifies from them and the next event chains on the last. The function cannot check a
-- signature; verification finds a forged checkpoint, and pruning that no audit.pruned event
-- records.

-- +goose Up
CREATE TABLE audit_checkpoints (
    seq       bigint PRIMARY KEY CHECK (seq > 0),
    hash      text NOT NULL,
    at        timestamptz NOT NULL,
    key       text NOT NULL,
    signature text NOT NULL
);

CREATE TABLE audit_retention (
    only_row  boolean PRIMARY KEY DEFAULT true CHECK (only_row),
    retention interval NOT NULL CHECK (retention > interval '0')
);
-- A year of 365 days, audit.retention's default (an interval of '1 year' is 360 days).
INSERT INTO audit_retention (retention) VALUES (interval '8760 hours');

-- +goose StatementBegin
CREATE FUNCTION audit_prune() RETURNS TABLE (checkpoint bigint, events bigint)
LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE
    anchor  bigint;
    deleted bigint;
BEGIN
    -- The latest checkpoint matching its event, which is older than the retention.
    SELECT c.seq INTO anchor
    FROM audit_checkpoints c JOIN audit_events e ON e.seq = c.seq AND e.hash = c.hash
    WHERE e.at < now() - (SELECT r.retention FROM audit_retention r)
    ORDER BY c.seq DESC
    LIMIT 1;
    IF anchor IS NULL THEN
        RETURN QUERY SELECT NULL::bigint, 0::bigint;
        RETURN;
    END IF;
    DELETE FROM audit_personal p WHERE p.seq < anchor;
    DELETE FROM audit_events e WHERE e.seq < anchor;
    GET DIAGNOSTICS deleted = ROW_COUNT;
    DELETE FROM audit_checkpoints c WHERE c.seq < anchor;
    RETURN QUERY SELECT anchor, deleted;
END
$$;
-- +goose StatementEnd

-- A SECURITY DEFINER function runs with its own search path, the schema it was created in, so
-- that the caller's objects cannot stand in for the tables; pg_temp last.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER FUNCTION audit_prune() SET search_path = %I, pg_temp', current_schema());
END
$$;
-- +goose StatementEnd

-- Functions are executable by PUBLIC by default; quai migrate grants it to the application role.
REVOKE ALL ON FUNCTION audit_prune() FROM PUBLIC;

-- +goose Down
DROP FUNCTION audit_prune();
DROP TABLE audit_retention;
DROP TABLE audit_checkpoints;
