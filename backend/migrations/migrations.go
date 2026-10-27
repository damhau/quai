// Package migrations holds the database migrations, embedded in the binary and applied with
// goose (docs/engineering/backend.md). A migration is never edited once released; changes are
// new files.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
