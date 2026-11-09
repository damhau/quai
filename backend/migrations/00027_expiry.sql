-- Reminders, escalation and expiry (docs/design/05-delivery-and-approvals.md#reminders-escalation-expiry, P4-14).
--
-- submitted_at is when the request was submitted, by the service's clock (created_at is the
-- database's): reminders and escalation count from it. expiry is the approval expiry the
-- request's definitions set when it was planned, as written (7d; '' for none), and expires_at
-- when it expires, from submitted_at: kept with the request, so a later change of its type
-- neither expires nor spares a request that waits (open question 78). Requests submitted before
-- this migration have no expiry.
--
-- reminded_at, escalated_at and expiry_warned_at mark the steps done, each once, set under the
-- request's row lock in the transaction that records the step's event; the expiry is the state
-- expired itself.
--
-- reviews.voided_at: the request expired, and the review never counts again.

-- +goose Up
ALTER TABLE requests
    ADD COLUMN submitted_at     timestamptz,
    ADD COLUMN expiry           text NOT NULL DEFAULT '',
    ADD COLUMN expires_at       timestamptz,
    ADD COLUMN reminded_at      timestamptz,
    ADD COLUMN escalated_at     timestamptz,
    ADD COLUMN expiry_warned_at timestamptz;
UPDATE requests SET submitted_at = created_at;
ALTER TABLE requests ALTER COLUMN submitted_at SET NOT NULL, ALTER COLUMN submitted_at SET DEFAULT now();

CREATE INDEX requests_pending ON requests (submitted_at) WHERE state = 'pending_approval';

ALTER TABLE reviews ADD COLUMN voided_at timestamptz;

-- +goose Down
ALTER TABLE reviews DROP COLUMN voided_at;
DROP INDEX requests_pending;
ALTER TABLE requests
    DROP COLUMN expiry_warned_at, DROP COLUMN escalated_at, DROP COLUMN reminded_at,
    DROP COLUMN expires_at, DROP COLUMN expiry, DROP COLUMN submitted_at;
