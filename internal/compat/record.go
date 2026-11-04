// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"context"
	"fmt"
	"os"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// Record writes the release folder out with the writers of this build: the template configs, the
// home_checkpoints fixture for this build's newest schema version and a kopia home backup. It
// refuses a non-empty out, because a recorded release is never overwritten (docs/dev/ci.md).
func Record(ctx context.Context, out, release string) error {
	if release == "" {
		return fmt.Errorf("a release name is required")
	}
	if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
		return fmt.Errorf("%s is not empty: a recorded release is not overwritten", out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	schema, err := migrations.Latest()
	if err != nil {
		return err
	}
	if err := recordAgentConfigs(out); err != nil {
		return fmt.Errorf("agent configs: %w", err)
	}
	if err := recordHomeCheckpoints(out); err != nil {
		return fmt.Errorf("home checkpoints: %w", err)
	}
	if err := recordHomeBackup(ctx, out); err != nil {
		return fmt.Errorf("home backup: %w", err)
	}
	return writeJSON(out+"/"+ManifestFile, Manifest{Release: release, SchemaVersion: schema})
}
