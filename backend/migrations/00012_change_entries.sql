-- The changes to entries of a shared file that make a file change (the entry locator), so that
-- delivery can make them again on the file as it is then
-- (docs/design/03-file-editing.md#concurrency-and-fingerprints). NULL: the change replaces the
-- whole file.

-- +goose Up
ALTER TABLE changes ADD COLUMN entries jsonb;

-- +goose Down
ALTER TABLE changes DROP COLUMN entries;
