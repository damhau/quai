-- Separation of duties (docs/design/05-delivery-and-approvals.md#separation-of-duties, P4-12).
--
-- A request keeps the repository's separation-of-duties options it was planned with, and, with
-- definitionAuthors, who changed its definitions in that period before it: the period, when it
-- began, the definitions commit Quai had recorded then (none: the whole history counted), the
-- people excluded with the commits that matched them, and the identities that matched no one.
-- They are never computed again for the request (open questions 71 to 74).
--
-- Personal data: the people's names, and the unmatched identities' names and emails.

-- +goose Up
ALTER TABLE requests ADD COLUMN separation jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE requests DROP COLUMN separation;
