-- The owners the Git platform names for a request's paths, matched to Quai for the codeowners
-- approver kind, and those that match no one, shown on the request; the email a reviewer had
-- when they reviewed, which codeowners entries match (docs/design/08-identity-access-tenancy.md#directory).

-- +goose Up
ALTER TABLE requests ADD COLUMN codeowners text[] NOT NULL DEFAULT '{}';
ALTER TABLE requests ADD COLUMN codeowners_unmatched text[] NOT NULL DEFAULT '{}';
ALTER TABLE reviews ADD COLUMN reviewer_email text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE reviews DROP COLUMN reviewer_email;
ALTER TABLE requests DROP COLUMN codeowners_unmatched;
ALTER TABLE requests DROP COLUMN codeowners;
