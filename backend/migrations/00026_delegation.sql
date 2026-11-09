-- Delegation (docs/design/05-delivery-and-approvals.md#delegation, P4-13).
--
-- An approver hands their approvals to a named colleague for a period: from starts_at (inclusive)
-- to ends_at (exclusive), instants in UTC, at most 90 days, never in the past when made (open
-- question 24). Only the delegator makes one, for themselves; they or a platform admin revoke it
-- (revoked_at, revoked_by). A delegator has one delegate at a time, and nobody delegates to someone
-- who has delegated their own approvals for an overlapping period, so delegations never form a
-- cycle; what a delegate received is never passed on (open question 75).
--
-- A review made by a delegate keeps on whose behalf (on_behalf_of), the delegation it used, and
-- the delegator as the directory described them when the review was made (email, groups, teams):
-- the policy decides the review with the delegator's rights then, under the delegate's own
-- checks (never the requester nor a definition author). '' is a review in one's own right.
--
-- Personal data: the delegator's, the delegate's and the revoker's names; the delegator's email,
-- groups and teams on reviews.

-- +goose Up
CREATE TABLE delegations (
    id         uuid PRIMARY KEY,
    org_id     text NOT NULL DEFAULT 'default',
    delegator  text NOT NULL,
    delegate   text NOT NULL,
    starts_at  timestamptz NOT NULL,
    ends_at    timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    revoked_by text NOT NULL DEFAULT '',
    CHECK (delegator <> delegate),
    CHECK (starts_at < ends_at),
    CHECK (ends_at - starts_at <= interval '90 days'),
    -- Erasing someone from the directory erases the delegations they gave or received.
    FOREIGN KEY (org_id, delegator) REFERENCES users (org_id, name) ON DELETE CASCADE,
    FOREIGN KEY (org_id, delegate) REFERENCES users (org_id, name) ON DELETE CASCADE
);

CREATE INDEX delegations_delegator ON delegations (delegator, ends_at) WHERE revoked_at IS NULL;
CREATE INDEX delegations_delegate ON delegations (delegate, ends_at) WHERE revoked_at IS NULL;

ALTER TABLE reviews
    ADD COLUMN on_behalf_of     text NOT NULL DEFAULT '',
    ADD COLUMN delegation_id    uuid,
    ADD COLUMN on_behalf_email  text NOT NULL DEFAULT '',
    ADD COLUMN on_behalf_groups text[] NOT NULL DEFAULT '{}',
    ADD COLUMN on_behalf_teams  text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE reviews
    DROP COLUMN on_behalf_teams, DROP COLUMN on_behalf_groups, DROP COLUMN on_behalf_email,
    DROP COLUMN delegation_id, DROP COLUMN on_behalf_of;
DROP TABLE delegations;
