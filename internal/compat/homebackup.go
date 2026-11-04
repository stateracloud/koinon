// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

const (
	homeBackupDir  = "home_backup"
	homeBackupRepo = "repo"
	homeBackupMeta = "meta.json"
	// Fixed IDs: the repository prefix is orgs/<org>/homes/<agent>/.
	backupOrg   = "0198a000-0000-7000-8000-0000000000c1"
	backupAgent = "0198a000-0000-7000-8000-0000000000c2"
)

// homeFiles is the home the recorder snapshots: path to content.
var homeFiles = map[string]string{
	"notes/a.md":                    "first note\n",
	"notes/b.md":                    "second note\n",
	".local/share/opencode/s1.json": `{"session":"s1"}` + "\n",
}

type backupMeta struct {
	Prefix     string            `json:"prefix"`
	SnapshotID string            `json:"snapshotId"`
	Files      map[string]string `json:"files"`
}

func homeTar(files map[string]string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	// Stable order: directories first as they are met.
	sort.Strings(names)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, err
	}
	for _, n := range names {
		for d := filepath.Dir(n); d != "." && !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	var dnames []string
	for d := range dirs {
		dnames = append(dnames, d)
	}
	sort.Strings(dnames)
	for _, d := range dnames {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + d + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: "./" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}

// recordHomeBackup writes a kopia repository made by the current checkpoint.Writer.
func recordHomeBackup(ctx context.Context, out string) error {
	dir := filepath.Join(out, homeBackupDir)
	repo := filepath.Join(dir, homeBackupRepo)
	if err := os.MkdirAll(repo, 0o750); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "koinon-compat-record-")
	if err != nil {
		return err
	}
	defer checkpoint.RemoveAll(scratch)
	w := &checkpoint.Writer{Storage: checkpoint.Dir(repo), Scratch: scratch}
	prefix := checkpoint.Prefix(backupOrg, backupAgent)
	tr, err := homeTar(homeFiles)
	if err != nil {
		return err
	}
	res, err := w.Backup(ctx, prefix, Password, tr, checkpoint.Meta{CheckpointID: "compat", Generation: "compat", Epoch: 1})
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, homeBackupMeta), backupMeta{Prefix: prefix, SnapshotID: res.SnapshotID, Files: homeFiles})
}

// CheckHomeBackup restores the recorded snapshot with checkpoint.Writer.Restore and compares the
// files with what was recorded. The repository is copied first: opening one can write to it.
func CheckHomeBackup(ctx context.Context, dir string) error {
	var meta backupMeta
	b, err := os.ReadFile(filepath.Join(dir, homeBackupDir, homeBackupMeta)) //nolint:gosec // G304: path inside the recorded release folder
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return fmt.Errorf("%s: %w", homeBackupMeta, err)
	}
	work, err := os.MkdirTemp("", "koinon-compat-check-")
	if err != nil {
		return err
	}
	defer checkpoint.RemoveAll(work)
	repo := filepath.Join(work, "repo")
	if err := os.CopyFS(repo, os.DirFS(filepath.Join(dir, homeBackupDir, homeBackupRepo))); err != nil {
		return err
	}
	scratch := filepath.Join(work, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return err
	}
	w := &checkpoint.Writer{Storage: checkpoint.Dir(repo), Scratch: scratch}
	for _, id := range []string{meta.SnapshotID, ""} { // the named snapshot, and "newest"
		var buf bytes.Buffer
		if err := w.Restore(ctx, meta.Prefix, Password, id, &buf); err != nil {
			return fmt.Errorf("restoring snapshot %q: %w", id, err)
		}
		got, err := regularFiles(&buf)
		if err != nil {
			return err
		}
		if !maps.Equal(got, meta.Files) {
			return fmt.Errorf("restored home differs (snapshot %q):\n got  %v\n want %v", id, got, meta.Files)
		}
	}
	return nil
}

func regularFiles(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(h.Name, "./")] = string(body)
	}
}
