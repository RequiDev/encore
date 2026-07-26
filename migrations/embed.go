// Package migrations embeds Encore's forward-only SQL migrations so that every
// binary carries the exact schema it was built against. No file needs to be
// shipped alongside the container image.
package migrations

import "embed"

// FS holds the numbered migration files, applied in filename order by goose.
//
//go:embed *.sql
var FS embed.FS
