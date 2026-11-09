-- Break-glass (docs/design/05-delivery-and-approvals.md#break-glass, P4-17).
--
-- break_glass is the record of a request merged without its approvals by a member of the
-- break-glass role. It is written under the request's row lock, in the transaction that moves the
-- request to approved, before any call to the platform: who broke glass (breaker, with their email
-- and their authentication as the session had it), why (reason, incident), what (the change set's
-- hash and the pull request's head the breaker saw; '' in direct mode), from which state, and when
-- (at, by the service's clock). A request is broken once at most (open question 95).
--
-- Its post-incident review opens with it and closes (closed_at) at the second sign-off in
-- break_glass_sign_offs: two distinct people, each a platform admin, an auditor or an owner of
-- the request, never the breaker nor the requester (open questions 27 and 99). A sign-off is never
-- withdrawn. reminded_at is when the open review was last reminded.
--
-- requests.break_glass_at and break_glass_by say that the request merges as broken: its change
-- windows lifted, quai/approval "Break-glass by <name>". TransitionRequest clears them when the
-- request goes back to pending approval (its content changed): the break-glass was for the
-- content it recorded.
--
-- Personal data: the breaker's and the signers' names and emails, the reason and the comments.

-- +goose Up
CREATE TABLE break_glass (
    request_id      uuid PRIMARY KEY REFERENCES requests (id) ON DELETE CASCADE,
    org_id          text NOT NULL DEFAULT 'default',
    breaker         text NOT NULL,
    breaker_email   text NOT NULL DEFAULT '',
    auth_time       timestamptz,
    acr             text NOT NULL DEFAULT '',
    step_up         boolean NOT NULL DEFAULT false,
    reason          text NOT NULL CHECK (reason <> ''),
    incident        text NOT NULL CHECK (incident <> ''),
    change_set_hash text NOT NULL,
    head            text NOT NULL DEFAULT '',
    from_state      text NOT NULL,
    at              timestamptz NOT NULL,
    reminded_at     timestamptz,
    closed_at       timestamptz
);

CREATE INDEX break_glass_open ON break_glass (at) WHERE closed_at IS NULL;

CREATE TABLE break_glass_sign_offs (
    request_id   uuid NOT NULL REFERENCES break_glass (request_id) ON DELETE CASCADE,
    signer       text NOT NULL,
    signer_email text NOT NULL DEFAULT '',
    capacity     text NOT NULL CHECK (capacity IN ('platform-admin', 'auditor', 'owner')),
    comment      text NOT NULL CHECK (comment <> ''),
    auth_time    timestamptz,
    step_up      boolean NOT NULL DEFAULT false,
    at           timestamptz NOT NULL,
    PRIMARY KEY (request_id, signer)
);

ALTER TABLE requests
    ADD COLUMN break_glass_at timestamptz,
    ADD COLUMN break_glass_by text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE requests DROP COLUMN break_glass_by, DROP COLUMN break_glass_at;
DROP TABLE break_glass_sign_offs;
DROP TABLE break_glass;
