// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the goose SQL migrations. File names are
// zero-padded (NNNNN_name.sql) so goose and sqlc read them in the same order.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
