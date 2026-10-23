// Package db embeds the registry schema snapshot and migrations (ADR-F0001).
package db

import (
	"embed"
	"io/fs"
)

// SchemaSQL is the snapshot of the latest schema (== all migrations applied).
// It must stay byte-identical to all migrations/*.sql concatenated in version
// order; see TestEmbeddedSchemaMatchesMigrations and `make check-schema`.
//
//go:embed schema.sql
var SchemaSQL string

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migration files (NNNN_name.sql) at the FS root.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded path is static
	}
	return sub
}
