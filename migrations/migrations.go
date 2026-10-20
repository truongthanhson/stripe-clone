// Package migrations embeds the SQL schema migrations so they ship inside the binary.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
