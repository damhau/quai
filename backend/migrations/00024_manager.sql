-- Manager approvers (docs/design/05-delivery-and-approvals.md#approver-kinds, P4-11).
--
-- The directory keeps each person's manager as their last sign-in found it: the identity
-- provider's immutable identifier of the manager (a claim's sub or Entra ID oid, or Microsoft
-- Graph's user id, which is the oid), where it came from (claim or graph; scim from P4-15), and,
-- when there is none, why (no claim, Graph answered no manager, the lookup failed). A sign-in
-- always replaces them: a failed lookup leaves no manager, never the one found before.
--
-- A request keeps its requester's manager as submission resolved it to a person of the directory,
-- or none and why; it is never resolved again (open question 70).
--
-- Personal data: the manager's identifier and name.

-- +goose Up
ALTER TABLE users
    ADD COLUMN manager_id text NOT NULL DEFAULT '',
    ADD COLUMN manager_source text NOT NULL DEFAULT '',
    ADD COLUMN manager_reason text NOT NULL DEFAULT '';
ALTER TABLE requests
    ADD COLUMN manager text NOT NULL DEFAULT '',
    ADD COLUMN manager_reason text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE requests DROP COLUMN manager_reason, DROP COLUMN manager;
ALTER TABLE users DROP COLUMN manager_reason, DROP COLUMN manager_source, DROP COLUMN manager_id;
