-- A migrate request applies a type's migration to its non-conforming resources, in one change
-- set (docs/design/02-resource-types-and-generators.md#schema-evolution). Its target is the type,
-- repository/type, so a type has one open migrate request at a time.

-- +goose Up
ALTER TABLE requests DROP CONSTRAINT requests_kind_check;
ALTER TABLE requests ADD CONSTRAINT requests_kind_check CHECK (kind IN ('create', 'update', 'delete', 'action', 'migrate'));

-- +goose Down
DELETE FROM requests WHERE kind = 'migrate';
ALTER TABLE requests DROP CONSTRAINT requests_kind_check;
ALTER TABLE requests ADD CONSTRAINT requests_kind_check CHECK (kind IN ('create', 'update', 'delete', 'action'));
