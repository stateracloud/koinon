// SPDX-License-Identifier: Apache-2.0

// Package savedwork is the uncommitted work saved from a stuck sandbox (plan item AG1, ADR 0054).
//
// When a sandbox is found stuck, koinon-agentd reads the workspace's uncommitted changes (the
// working tree against the newest commit GitHub has, untracked files included) and seals them in
// Postgres with the envelope secret store: the files are packed into one gzip-compressed tar,
// and the tar is sealed as a value bound to the saved_work row. Postgres holds it because the
// work is at most 50 MB and lives 14 days; a blob store would add a dependency for that.
//
// The manifest (repository, base commit, and per file the path, status, mode and line counts) is
// stored beside the sealed files in the clear: the approval card shows it as a diffstat, and it
// holds no file content. Nothing reaches GitHub without an approval (package approvals).
package savedwork

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// Limits.
const (
	// MaxBytes bounds the files' total size before sealing.
	MaxBytes = 50 << 20
	// Retention is how long saved work is kept.
	Retention = 14 * 24 * time.Hour
	// BranchPrefix starts every branch saved work is pushed to. The default branch never gets it.
	BranchPrefix = "koinon/wip/"
	// MaxFiles bounds the files of one saved repository.
	MaxFiles = 5000
	// MaxNotes and MaxNoteBytes bound the lines the sandbox reports about what it left out: they
	// are shown to people, so a sandbox cannot flood a timeline or a card.
	MaxNotes     = 20
	MaxNoteBytes = 200
)

// Table and Column name the sealed value for the secret store's binding and the key tool.
const (
	Table  = "saved_work"
	Column = "envelope"
)

// Binding is where a saved work's envelope lives.
func Binding(org, id string) secretstore.Binding {
	return secretstore.Binding{OrgID: org, Table: Table, RowID: id, Column: Column}
}

// File statuses.
const (
	Added    = "A"
	Modified = "M"
	Deleted  = "D"
)

// File is one changed file. Mode is the git mode of the new version ("" when deleted).
type File struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// Manifest describes saved work without its content.
type Manifest struct {
	// Repo is the GitHub repository as owner/name; Base the commit the changes apply to.
	Repo  string `json:"repo"`
	Base  string `json:"base"`
	Files []File `json:"files"`
	// Skipped names changes that were not saved (symbolic links, submodules), with the reason.
	Skipped []string `json:"skipped,omitempty"`
}

// Bytes is the total size of the files.
func (m Manifest) Bytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Totals are the lines added and deleted over all files.
func (m Manifest) Totals() (added, deleted int) {
	for _, f := range m.Files {
		added += f.Added
		deleted += f.Deleted
	}
	return added, deleted
}

var (
	hashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hashPattern32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

var pathPattern = regexp.MustCompile(`^[^\x00-\x1f\x7f]+$`)

// ValidPath reports whether p is a repository-relative path Koinon pushes: no leading slash, no
// "." or ".." segment, no ".git" segment, no control characters.
func ValidPath(p string) bool {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || !pathPattern.MatchString(p) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

// Validate checks a manifest.
func (m Manifest) Validate() error {
	if len(m.Files) == 0 {
		return errors.New("savedwork: no files")
	}
	if len(m.Files) > MaxFiles {
		return fmt.Errorf("savedwork: more than %d files", MaxFiles)
	}
	if len(m.Skipped) > MaxNotes {
		return fmt.Errorf("savedwork: more than %d notes", MaxNotes)
	}
	for _, n := range m.Skipped {
		if len(n) > MaxNoteBytes {
			return errors.New("savedwork: a note is too long")
		}
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		switch {
		case !ValidPath(f.Path):
			return fmt.Errorf("savedwork: invalid path %q", f.Path)
		case seen[f.Path]:
			return fmt.Errorf("savedwork: %q twice", f.Path)
		case f.Status == Deleted && f.Mode != "":
			return fmt.Errorf("savedwork: deleted %q has a mode", f.Path)
		case f.Status != Deleted && f.Mode != "100644" && f.Mode != "100755":
			return fmt.Errorf("savedwork: %q has mode %q", f.Path, f.Mode)
		case f.Status != Added && f.Status != Modified && f.Status != Deleted:
			return fmt.Errorf("savedwork: %q has status %q", f.Path, f.Status)
		}
		seen[f.Path] = true
	}
	if m.Bytes() > MaxBytes {
		return fmt.Errorf("savedwork: %d bytes is over the %d byte limit", m.Bytes(), MaxBytes)
	}
	return nil
}

// Work is a manifest and the content of the files that are not deleted, by path.
type Work struct {
	Manifest Manifest
	Content  map[string][]byte
}

// Pack writes w as a gzip-compressed tar: manifest.json first, then each file as files/<path>.
func Pack(w Work) ([]byte, error) {
	if err := w.Manifest.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	mj, err := json.Marshal(w.Manifest)
	if err != nil {
		return nil, err
	}
	if err := add("manifest.json", mj); err != nil {
		return nil, err
	}
	for _, f := range w.Manifest.Files {
		if f.Status == Deleted {
			continue
		}
		c, ok := w.Content[f.Path]
		if !ok || int64(len(c)) != f.Size {
			return nil, fmt.Errorf("savedwork: content of %q is missing or its size differs from the manifest", f.Path)
		}
		if err := add("files/"+f.Path, c); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack reads what Pack wrote. It reads at most MaxBytes of files and checks the manifest, so a
// tampered archive cannot make a push do anything the manifest does not say.
func Unpack(packed []byte) (Work, error) {
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return Work{}, fmt.Errorf("savedwork: %w", err)
	}
	tr := tar.NewReader(io.LimitReader(zr, MaxBytes+(8<<20)))
	var w Work
	w.Content = map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Work{}, fmt.Errorf("savedwork: %w", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return Work{}, fmt.Errorf("savedwork: %w", err)
		}
		if h.Name == "manifest.json" {
			if err := json.Unmarshal(b, &w.Manifest); err != nil {
				return Work{}, fmt.Errorf("savedwork: manifest: %w", err)
			}
			continue
		}
		if p, ok := strings.CutPrefix(h.Name, "files/"); ok {
			w.Content[p] = b
		}
	}
	if err := w.Manifest.Validate(); err != nil {
		return Work{}, err
	}
	for _, f := range w.Manifest.Files {
		if f.Status != Deleted && int64(len(w.Content[f.Path])) != f.Size {
			return Work{}, fmt.Errorf("savedwork: content of %q does not match the manifest", f.Path)
		}
	}
	return w, nil
}

// Seal packs w and seals it for the saved_work row (org, id).
func Seal(ring *secretstore.Keyring, org, id string, w Work) (secretstore.Envelope, error) {
	packed, err := Pack(w)
	if err != nil {
		return secretstore.Envelope{}, err
	}
	return SealPacked(ring, org, id, packed)
}

// SealPacked seals bytes Pack wrote.
func SealPacked(ring *secretstore.Keyring, org, id string, packed []byte) (secretstore.Envelope, error) {
	return ring.Seal(Binding(org, id), packed)
}

// Open opens the envelope of the saved_work row (org, id) and unpacks it.
func Open(ring *secretstore.Keyring, org, id string, env secretstore.Envelope) (Work, error) {
	w, _, err := OpenPacked(ring, org, id, env)
	return w, err
}

// OpenPacked is Open that also returns the packed bytes, whose SHA-256 the approval binds to.
func OpenPacked(ring *secretstore.Keyring, org, id string, env secretstore.Envelope) (Work, []byte, error) {
	packed, err := ring.Open(Binding(org, id), env)
	if err != nil {
		return Work{}, nil, err
	}
	w, err := Unpack(packed)
	return w, packed, err
}

// BundleHash is the SHA-256 (hex) of packed bytes; ManifestHash that of the manifest's JSON.
func BundleHash(packed []byte) string {
	sum := sha256.Sum256(packed)
	return hex.EncodeToString(sum[:])
}

// ManifestHash is the SHA-256 (hex) of the manifest as JSON.
func ManifestHash(m Manifest) string {
	b, _ := json.Marshal(m) //nolint:errchkjson // plain values
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BranchName is the branch saved work for a ticket is pushed to: koinon/wip/<ticket>-<time>, the
// ticket's first 8 hex digits and the time in UTC. A repository's n-th saved work of the incident
// gets a numeric suffix from the caller when it must be told apart.
func BranchName(ticketID string, t time.Time) string {
	short := strings.ReplaceAll(ticketID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return BranchPrefix + short + "-" + t.UTC().Format("20060102T150405Z")
}

// ValidBranch reports whether b is a branch name Koinon pushes saved work to.
func ValidBranch(b string) bool {
	rest, ok := strings.CutPrefix(b, BranchPrefix)
	return ok && rest != "" && len(b) <= 200 && regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(rest) &&
		!strings.HasSuffix(b, ".lock") && !strings.Contains(b, "..")
}

// ToolPush is the tool of the approval request that asks to push saved work: connector "github",
// target "github:owner/name". The request is made by Koinon itself (approval_requests.system).
const ToolPush = "push_saved_work"

// PushArgsJSON is the canonical arguments of a push request: sorted keys, no insignificant space.
// The approval binds to its SHA-256.
func PushArgsJSON(savedWorkID, repo, base, branch, bundleSHA, manifestSHA string) string {
	b, _ := json.Marshal(map[string]string{ //nolint:errchkjson // strings only; encoding/json sorts the keys
		"base": base, "branch": branch, "bundle_sha256": bundleSHA, "manifest_sha256": manifestSHA,
		"repo": repo, "saved_work_id": savedWorkID,
	})
	return string(b)
}

// PushArgs are the arguments of a push request.
type PushArgs struct {
	SavedWorkID string `json:"saved_work_id"`
	Repo        string `json:"repo"`
	Base        string `json:"base"`
	Branch      string `json:"branch"`
	// BundleSHA256 and ManifestSHA256 bind the approval to the content: the packed files and the
	// manifest the approver saw. The push refuses anything else.
	BundleSHA256   string `json:"bundle_sha256"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// DecodePushArgs reads and checks a push request's recorded arguments.
func DecodePushArgs(argsJSON string) (PushArgs, error) {
	var a PushArgs
	dec := json.NewDecoder(strings.NewReader(argsJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return PushArgs{}, fmt.Errorf("savedwork: arguments: %w", err)
	}
	if a.SavedWorkID == "" || a.Repo == "" || a.Base == "" || !ValidBranch(a.Branch) ||
		!hashPattern.MatchString(a.BundleSHA256) || !hashPattern.MatchString(a.ManifestSHA256) {
		return PushArgs{}, errors.New("savedwork: incomplete arguments")
	}
	return a, nil
}
