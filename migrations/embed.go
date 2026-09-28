// Package migrations embeds the versioned SQL schema so the migration runner
// has no filesystem dependency at runtime. The files are plain SQL and stay
// readable and editable on disk; this package only carries them into the
// binary.
package migrations

import "embed"

// FS holds every NNN_name.up.sql / NNN_name.down.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
