-- When the database recorded each audit event, which pruning judges its age by
-- (docs/design/09-security-and-compliance.md#audit-log, open question 36).
--
-- An event's at is the application's: the application role inserts it, and could date an event
-- years back, sign or forge a checkpoint of it, and have audit_prune() delete every event before
-- it. recorded is the database's clock when the row was inserted: quai migrate grants the
-- application role INSERT on the other columns only, and no UPDATE, so it can neither set it nor
-- change it. It is not hashed nor exported: the chain is the application's account of what
-- happened, recorded the database's of when it learned of it.
--
-- New rows take clock_timestamp(), not the transaction's start: events are inserted in seq order
-- under the audit log's lock, so recorded follows seq, and a long transaction cannot make its
-- event older than those before it. Rows already there take this migration's time: nothing
-- written before counts as old until a retention has passed from now.

-- +goose Up
ALTER TABLE audit_events ADD COLUMN recorded timestamptz NOT NULL DEFAULT now();
ALTER TABLE audit_events ALTER COLUMN recorded SET DEFAULT clock_timestamp();

-- CREATE OR REPLACE keeps the function's owner and grants, and resets the rest: SECURITY DEFINER
-- is said again, and the search path set again below.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_prune() RETURNS TABLE (checkpoint bigint, events bigint)
LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE
    anchor  bigint;
    deleted bigint;
BEGIN
    -- The latest checkpoint matching its event, which the database recorded before the retention.
    SELECT c.seq INTO anchor
    FROM audit_checkpoints c JOIN audit_events e ON e.seq = c.seq AND e.hash = c.hash
    WHERE e.recorded < now() - (SELECT r.retention FROM audit_retention r)
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

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER FUNCTION audit_prune() SET search_path = %I, pg_temp', current_schema());
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION audit_prune() FROM PUBLIC;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_prune() RETURNS TABLE (checkpoint bigint, events bigint)
LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE
    anchor  bigint;
    deleted bigint;
BEGIN
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

-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER FUNCTION audit_prune() SET search_path = %I, pg_temp', current_schema());
END
$$;
-- +goose StatementEnd

ALTER TABLE audit_events DROP COLUMN recorded;
