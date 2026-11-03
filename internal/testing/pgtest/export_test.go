// SPDX-License-Identifier: Apache-2.0

package pgtest

import (
	"context"
	"io/fs"
)

// TemplateBuilds is how many template databases this process migrated.
func TemplateBuilds() int64 { return templateBuilds.Load() }

// EnsureTemplate builds the template database name on the server at adminDSN unless it is there.
func EnsureTemplate(ctx context.Context, adminDSN, name string) error {
	return ensureTemplate(ctx, adminDSN, name)
}

// ServerDSN is the superuser DSN of the maintenance database of t's server.
func (t *Template) ServerDSN() string { return t.s.dsn }

// ProcID is the part of database names unique to this process.
func ProcID() string { return procID() }

// HashWithGoose is MigrationsHash for a given goose version.
func HashWithGoose(fsys fs.FS, goose string) (string, error) { return migrationsHashFor(fsys, goose) }

// GooseVersion is the goose version MigrationsHash uses.
func GooseVersion() (string, error) { return gooseVersion() }
