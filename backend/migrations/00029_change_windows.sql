-- Change windows and freezes (docs/design/05-delivery-and-approvals.md#change-windows-and-freezes, P4-16).
--
-- window_subjects is what a change window's when sees of each of the request's resources (its
-- type and params), kept from its plan: [{"type": "vm", "params": {"env": "prod", ...}}]. NULL for
-- requests planned before this migration: every window with a when applies to them.
--
-- window_held_at is when Quai began to hold an approved request for its change windows: its
-- quai/approval pending with the reason, no merge or commit until they open. Set and cleared
-- under the request's row lock in the transaction that records the event (window_waiting,
-- window_opened); NULL when it is not held. window_reason is the reason its quai/approval gives
-- meanwhile ("Approved; waits for the change window, opens Mon 08:00 Europe/Zurich"), for the
-- windows' sweep to publish it again when it changes (a freeze added while it waits); '' when it
-- is not held.

-- +goose Up
ALTER TABLE requests
    ADD COLUMN window_subjects jsonb,
    ADD COLUMN window_held_at  timestamptz,
    ADD COLUMN window_reason   text NOT NULL DEFAULT '';

CREATE INDEX requests_approved ON requests (updated_at) WHERE state IN ('approved', 'merging');

-- +goose Down
DROP INDEX requests_approved;
ALTER TABLE requests DROP COLUMN window_reason, DROP COLUMN window_held_at, DROP COLUMN window_subjects;
