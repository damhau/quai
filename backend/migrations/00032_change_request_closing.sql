-- Closing ServiceNow change requests with the evidence, and break-glass incidents checked against
-- ServiceNow (docs/design/05-delivery-and-approvals.md#itsm, P4-20).
--
-- A request's change request is closed once the content it is for is applied (close_outcome
-- successful) or its apply failed (unsuccessful): close_requested_at is set in the transaction of
-- that transition, on the current change request only, for the change set that landed; a
-- superseded one is never closed. Closing takes steps that each survive a crash: the evidence
-- bundle attached (change_request_attachments, a file at a time: its bytes and hash recorded
-- before the upload, its sys_id once ServiceNow answered or a lookup by its name found it), then
-- the state moved Implement, Review, Closed, each step read from ServiceNow first, so that a retry
-- moves nothing twice. evidence_token is the random part of the files' names
-- (quai-evidence-<number>-<token>.json), so that no one can attach a file before Quai that it
-- would take for its own. closed_at is when Quai was done, close_result how: closed by Quai,
-- already closed or canceled in ServiceNow, left (never approved: the evidence attached and a work
-- note, its state left to the change manager), or refused (ServiceNow refused a move: problem says
-- what, and it is not tried again).
--
-- break_glass.incident_check is what ServiceNow said of the incident when itsm.servicenow is
-- configured ('' without): verified (an incident with that number, open or resolved), unverified
-- (ServiceNow could not say: break-glass went ahead, and Quai asks again), or refuted (ServiceNow
-- says no such incident, two, or closed: incident_problem says what). incident_sys_id is the
-- incident ServiceNow answered, checked as a sys_id; incident_link is the link of the incident to
-- the change request (its rfc field): linked, kept (it names another change request, left as it
-- is), none (no change request to link), or refused. incident_failure, incident_failures and
-- incident_next_at are the tries that did not reach ServiceNow since the last that did, and when to
-- try again.

-- +goose Up
ALTER TABLE change_requests
    ADD COLUMN close_outcome      text NOT NULL DEFAULT '' CHECK (close_outcome IN ('', 'successful', 'unsuccessful')),
    ADD COLUMN close_requested_at timestamptz,
    ADD COLUMN evidence_token     text NOT NULL DEFAULT '',
    ADD COLUMN closed_at          timestamptz,
    ADD COLUMN close_result       text NOT NULL DEFAULT '' CHECK (close_result IN ('', 'closed', 'already', 'left', 'refused'));

CREATE TABLE change_request_attachments (
    change_request_id uuid NOT NULL REFERENCES change_requests (id) ON DELETE CASCADE,
    name              text NOT NULL,
    bytes             integer NOT NULL CHECK (bytes >= 0),
    sha256            text NOT NULL,
    sys_id            text,
    attached_at       timestamptz,
    PRIMARY KEY (change_request_id, name)
);

ALTER TABLE break_glass
    ADD COLUMN incident_check      text NOT NULL DEFAULT '' CHECK (incident_check IN ('', 'verified', 'unverified', 'refuted')),
    ADD COLUMN incident_sys_id     text NOT NULL DEFAULT '',
    ADD COLUMN incident_checked_at timestamptz,
    ADD COLUMN incident_link       text NOT NULL DEFAULT '' CHECK (incident_link IN ('', 'linked', 'kept', 'none', 'refused')),
    ADD COLUMN incident_linked_at  timestamptz,
    ADD COLUMN incident_problem    text NOT NULL DEFAULT '',
    ADD COLUMN incident_failure    text NOT NULL DEFAULT '',
    ADD COLUMN incident_failures   integer NOT NULL DEFAULT 0,
    ADD COLUMN incident_next_at    timestamptz;

-- +goose Down
ALTER TABLE break_glass
    DROP COLUMN incident_check,
    DROP COLUMN incident_sys_id,
    DROP COLUMN incident_checked_at,
    DROP COLUMN incident_link,
    DROP COLUMN incident_linked_at,
    DROP COLUMN incident_problem,
    DROP COLUMN incident_failure,
    DROP COLUMN incident_failures,
    DROP COLUMN incident_next_at;

DROP TABLE change_request_attachments;

ALTER TABLE change_requests
    DROP COLUMN close_outcome,
    DROP COLUMN close_requested_at,
    DROP COLUMN evidence_token,
    DROP COLUMN closed_at,
    DROP COLUMN close_result;
