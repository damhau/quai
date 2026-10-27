-- What a request needs from submission to delivery: a number people can say ("#1042"), the
-- requester's email for the commit author, the stack it changes, and the approval rules matched
-- at submission with the owners they were matched against, so that reviews are decided against
-- what was approved for, not against definitions that changed since.

-- +goose Up
ALTER TABLE requests
    ADD COLUMN number          bigint NOT NULL GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN requester_email text NOT NULL DEFAULT '',
    ADD COLUMN stack           text NOT NULL DEFAULT '',
    ADD COLUMN rules           jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN owners          text[] NOT NULL DEFAULT '{}';
CREATE UNIQUE INDEX requests_number ON requests (org_id, number);

-- +goose Down
DROP INDEX requests_number;
ALTER TABLE requests DROP COLUMN number, DROP COLUMN requester_email, DROP COLUMN stack, DROP COLUMN rules, DROP COLUMN owners;
