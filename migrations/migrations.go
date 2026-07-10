// Package migrations embeds Purser's SQL migration files so the store's
// migrator (internal/store) can apply them at runtime on boot.
package migrations

import "embed"

// FS holds every *.up.sql / *.down.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
