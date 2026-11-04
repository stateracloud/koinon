// SPDX-License-Identifier: Apache-2.0

// Package compat is the persisted-state compatibility check (#652): what a shipped release wrote
// to storage must still load at head. A recorded release is a folder testdata/compat/<release>/
// made by hack/record-compat-fixtures.sh from the code of that release; Check* load it with the
// code at head. The three formats are the stored agent configs (agent_versions.config), the
// home_checkpoints rows (after migrating from the recorded schema version) and a kopia home backup.
//
// A folder is never edited by hand: a format change that breaks one is either a bug or a
// deliberate break that needs a migration or a reader for the old shape (docs/dev/ci.md).
package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Root is the folder, relative to the repository root, that holds one subfolder per release.
const Root = "testdata/compat"

// Password opens the recorded kopia repository. It is obviously fake: the repository holds only
// the fixture files.
const Password = "koinon_test_not-a-real-password"

// ManifestFile names the manifest at the top of a release folder.
const ManifestFile = "manifest.json"

// Manifest describes one recorded release folder.
type Manifest struct {
	// Release is the tag or name the folder was recorded for, e.g. v0.1.0.
	Release string `json:"release"`
	// SchemaVersion is the goose version of the schema the home_checkpoints fixture was written for.
	SchemaVersion int64 `json:"schemaVersion"`
}

// Releases lists the recorded release folders under root, sorted. A missing root is no releases.
func Releases(root string) ([]string, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ReadManifest reads the manifest of the release folder dir.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // G304: path inside the recorded release folder
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if m.Release == "" || m.SchemaVersion < 1 {
		return m, fmt.Errorf("%s: release and schemaVersion are required", ManifestFile)
	}
	return m, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// JSONContains reports whether every key and value of want appears unchanged in got. Objects are
// compared key by key and arrays element by element; keys only got has are allowed, so a field a
// later release adds to a decoded config does not fail a recorded folder.
func JSONContains(got, want []byte) (bool, error) {
	var x, y any
	if err := json.Unmarshal(got, &x); err != nil {
		return false, err
	}
	if err := json.Unmarshal(want, &y); err != nil {
		return false, err
	}
	return subset(x, y), nil
}

func subset(got, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !subset(gv, wv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !subset(g[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}
