// Package migrations embeds the goose SQL migrations applied by cmd/recon and
// by the integration-test template.
package migrations

import "embed"

// FS holds the goose migration files.
//
//go:embed *.sql
var FS embed.FS
