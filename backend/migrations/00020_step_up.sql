-- Step-up authentication (docs/design/08-identity-access-tenancy.md#step-up, P4-06).
--
-- A request needs step-up approvals when, at submission, the stepUp condition of an approval block
-- that applies to it held (open question 50): it is decided once, with the request's rules.
-- Requests from before have none.
--
-- A review keeps how its reviewer had authenticated when they reviewed, as it keeps their
-- authentication time: the ID token's acr and Entra ID's acrs, and whether that authentication
-- satisfied the step-up then. On a step-up request, only those reviews count; the session's later
-- state does not change them.

-- +goose Up
ALTER TABLE requests ADD COLUMN step_up boolean NOT NULL DEFAULT false;
ALTER TABLE reviews
    ADD COLUMN reviewer_acr text NOT NULL DEFAULT '',
    ADD COLUMN reviewer_acrs text[] NOT NULL DEFAULT '{}',
    ADD COLUMN step_up boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE reviews DROP COLUMN step_up, DROP COLUMN reviewer_acrs, DROP COLUMN reviewer_acr;
ALTER TABLE requests DROP COLUMN step_up;
