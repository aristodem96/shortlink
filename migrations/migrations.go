// Package migrations embeds the SQL schema so the binary can migrate a
// database on its own, with no migration tool installed next to it.
package migrations

import "embed"

// FS holds the goose migration files, in order.
//
//go:embed *.sql
var FS embed.FS
