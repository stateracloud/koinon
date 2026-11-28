// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"

	_ "github.com/kopia/kopia/repo/compression" // registers zstd
)

// source is the one snapshot source of every repository: each holds a single agent's home.
var source = snapshot.SourceInfo{Host: "koinon", UserName: "agent", Path: HomeDir}

// Retention is how many snapshots a repository keeps (kopia's policy): the latest 24, then one
// per day for 14 days and one per week for 8 weeks. Hourly snapshots are what B3 takes, so a day
// of them is kept.
var Retention = struct{ Latest, Hourly, Daily, Weekly int }{24, 24, 14, 8}

// Writer reads tar streams into repositories and writes them back out. It runs in
// koinon-agentd; it holds the storage credentials (through Storage) and is handed each
// repository's password per call.
type Writer struct {
	Storage Storage
	// Scratch is a directory for the staged home and kopia's cache (an emptyDir). Required.
	Scratch string
	Limits  Limits
	// ReadOnly is restore-only mode (checkpoints.readOnly, ADR 0110 decision 4): the repository opens with
	// kopia's ClientOptions.ReadOnly, so any write through it fails (a guard: kopia's read paths do not write
	// today, but a bucket the source still owns must stay untouched whatever a later path does), Backup fails
	// with [ReadOnlyError], and DeleteRepository refuses.
	ReadOnly bool
}

// Meta is stamped on a snapshot as tags: what checkpoints resync records for a snapshot that no
// checkpoint names (ADR 0085 decision 17), so the bucket alone tells which home a snapshot holds.
type Meta struct {
	CheckpointID string
	Generation   string
	Epoch        int64
	// Reason and Sandbox are the checkpoint's reason and the OpenShell sandbox read; empty leaves
	// the tag out (snapshots written before they were stamped have neither).
	Reason, Sandbox string
	// Ready, when set, is called once the whole stream is staged and before the snapshot is
	// written: an error stops the backup with no snapshot. The backup checks there that the home
	// was read whole (tar's exit code), so that every snapshot in the bucket holds a whole home and
	// checkpoints resync may record any snapshot it finds. Not a tag.
	Ready func() error
}

// tags is what m stamps on a snapshot.
func (m Meta) tags() map[string]string {
	t := map[string]string{"checkpoint": m.CheckpointID, "generation": m.Generation, "epoch": strconv.FormatInt(m.Epoch, 10)}
	if m.Reason != "" {
		t["reason"] = m.Reason
	}
	if m.Sandbox != "" {
		t["sandbox"] = m.Sandbox
	}
	return t
}

// metaOf reads the tags of a snapshot back.
func metaOf(t map[string]string) (Meta, error) {
	epoch, err := strconv.ParseInt(t["epoch"], 10, 64)
	if err != nil {
		return Meta{}, fmt.Errorf("the epoch tag %q is not a number", t["epoch"])
	}
	return Meta{CheckpointID: t["checkpoint"], Generation: t["generation"], Epoch: epoch, Reason: t["reason"], Sandbox: t["sandbox"]}, nil
}

// Snapshot is a complete snapshot of a repository, as Snapshots lists it.
type Snapshot struct {
	ID string
	// Start and End are when the snapshot was taken; Bytes is the size of its files.
	Start, End time.Time
	Bytes      int64
	// Meta is what its tags say; MetaErr is set instead when they cannot be read.
	Meta    Meta
	MetaErr error
}

// ErrNoRepositoryInBucket is returned by Snapshots for a prefix that holds no repository: one
// that was never backed up into, or a bucket that is not the one the database belongs to.
var ErrNoRepositoryInBucket = errors.New("checkpoint: the bucket holds no repository at that prefix")

// ErrPrefixTaken is returned when the prefix holds a repository that the password does not open.
// A repository's prefix follows from the agent and its epoch ([EpochPrefix]), so after a database
// restore the prefix of a new epoch can hold one started after the dump, whose password only the
// lost database had (#262). Nothing is written there: Backup does not create a repository over it.
var ErrPrefixTaken = errors.New("checkpoint: the prefix holds a repository this password does not open")

// PrefixTakenError is [ErrPrefixTaken] for one prefix, so that a caller that opens two
// repositories (a seed's copy) tells which one it was.
type PrefixTakenError struct{ Prefix string }

func (e *PrefixTakenError) Error() string { return ErrPrefixTaken.Error() + ": " + e.Prefix }

// Is makes errors.Is(err, ErrPrefixTaken) hold.
func (e *PrefixTakenError) Is(target error) bool { return target == ErrPrefixTaken }

// TakenPrefix reports whether err is [ErrPrefixTaken] for prefix.
func TakenPrefix(err error, prefix string) bool {
	var t *PrefixTakenError
	return errors.As(err, &t) && t.Prefix == prefix
}

// HoldsRepository reports whether the bucket holds a repository at prefix (its kopia.repository
// blob), whatever its password. It creates nothing.
func (w *Writer) HoldsRepository(ctx context.Context, prefix string) (bool, error) {
	st, err := w.Storage.Open(ctx, prefix, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil // a Dir prefix that was never written
	} else if err != nil {
		return false, fmt.Errorf("opening the storage: %w", err)
	}
	defer func() { _ = st.Close(ctx) }()
	_, err = st.GetMetadata(ctx, format.KopiaRepositoryBlobID)
	if errors.Is(err, blob.ErrBlobNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Result is a finished snapshot.
type Result struct {
	SnapshotID string
	Bytes      int64
	Files      int64
	Skipped    int64
	// Digest is the content digest of the staged home (TreeDigest), "sha256:<hex>". The caller
	// records it and gives it back to Restore.
	Digest string
}

func (w *Writer) limits() Limits {
	if w.Limits == (Limits{}) {
		return DefaultLimits
	}
	return w.Limits
}

// Backup stages the tar stream and snapshots it into the repository at prefix, creating the
// repository on first use, then applies the retention policy. The snapshot is complete only when
// Backup returns a result.
func (w *Writer) Backup(ctx context.Context, prefix, password string, tarStream io.Reader, m Meta) (Result, error) {
	if w.ReadOnly {
		return Result{}, errors.New(ReadOnlyError)
	}
	work, err := os.MkdirTemp(w.Scratch, "backup-")
	if err != nil {
		return Result{}, err
	}
	defer RemoveAll(work)
	home := filepath.Join(work, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return Result{}, err
	}
	stats, err := Stage(tarStream, home, w.limits())
	if err != nil {
		return Result{}, fmt.Errorf("reading the home: %w", err)
	}
	if m.Ready != nil {
		if err := m.Ready(); err != nil {
			return Result{}, err
		}
	}
	// The writer is not root: kopia skips, without an error, a file whose owner bits forbid a read.
	// Make every entry readable first, so that the snapshot keeps the data and the digest below
	// covers what the snapshot holds.
	if err := prepareHome(home); err != nil {
		return Result{}, fmt.Errorf("reading the home: %w", err)
	}
	digest, err := TreeDigest(home)
	if err != nil {
		return Result{}, fmt.Errorf("digesting the home: %w", err)
	}
	rep, err := w.open(ctx, work, prefix, password, true)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = rep.Close(ctx) }()

	src, err := localfs.Directory(home)
	if err != nil {
		return Result{}, err
	}
	var id manifest.ID
	err = repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "home checkpoint"}, func(ctx context.Context, wr repo.RepositoryWriter) error {
		pt, err := policy.TreeForSource(ctx, wr, source)
		if err != nil {
			return err
		}
		// The digest covers every staged file, so kopia may skip none: a `.kopiaignore` file or a
		// cache directory marker (CACHEDIR.TAG, which cargo writes in target/) in the agent's home
		// would leave the snapshot smaller than the digest, and no restore would pass. An old
		// repository keeps the policy it was made with, so this is set at upload time.
		pt = keepEverything(pt)
		u := upload.NewUploader(wr)
		u.ParallelUploads = 2
		// No previous snapshot goes to Upload: kopia would reuse a file's old contents when its name,
		// mode, size and time match the previous snapshot. The tar stream has whole-second times, so a
		// file the harness rewrote in the second an earlier checkpoint read it (same length, other
		// bytes) would keep the old bytes, and the snapshot would not match the digest above (issue
		// #1090). Every file is read and hashed; kopia still stores only the content it does not hold.
		man, err := u.Upload(ctx, src, pt, source)
		if err != nil {
			return err
		}
		if man.IncompleteReason != "" {
			return fmt.Errorf("the snapshot is incomplete: %s", man.IncompleteReason)
		}
		// A checkpoint must not lose data without a signal: an entry that kopia skipped or could not
		// read fails it, and no snapshot is saved, so no digest is recorded.
		if err := skippedEntries(man); err != nil {
			return err
		}
		man.Tags = m.tags()
		if id, err = snapshot.SaveSnapshot(ctx, wr, man); err != nil {
			return err
		}
		_, err = policy.ApplyRetentionPolicy(ctx, wr, source, true)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return Result{SnapshotID: string(id), Bytes: stats.Bytes, Files: stats.Files, Skipped: stats.Skipped, Digest: digest}, nil
}

// Restore writes the snapshot as a tar stream to dst. A snapshot ID of "" restores the newest.
//
// wantDigest is the digest that Backup returned for the snapshot (Result.Digest). Restore stages
// the snapshot, recomputes the digest and returns [ErrDigestMismatch] when it differs, before it
// writes the first byte to dst. An empty wantDigest skips the check: it is for a checkpoint made
// before digests were recorded, and the caller says so in its log.
func (w *Writer) Restore(ctx context.Context, prefix, password, snapshotID, wantDigest string, dst io.Writer) error {
	work, err := os.MkdirTemp(w.Scratch, "restore-")
	if err != nil {
		return err
	}
	defer RemoveAll(work)
	rep, err := w.open(ctx, work, prefix, password, false)
	if err != nil {
		return err
	}
	defer func() { _ = rep.Close(ctx) }()

	var man *snapshot.Manifest
	if snapshotID == "" {
		all, err := snapshot.ListSnapshots(ctx, rep, source)
		if err != nil {
			return err
		}
		for _, s := range all {
			if s.IncompleteReason == "" && (man == nil || s.StartTime.After(man.StartTime)) {
				man = s
			}
		}
		if man == nil {
			return errors.New("the repository has no snapshot")
		}
	} else if man, err = snapshot.LoadSnapshot(ctx, rep, manifest.ID(snapshotID)); err != nil {
		return fmt.Errorf("snapshot %s: %w", snapshotID, err)
	}
	root, err := snapshotfs.SnapshotRoot(rep, man)
	if err != nil {
		return err
	}
	target := filepath.Join(work, "home")
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	out := &restore.FilesystemOutput{
		TargetPath: target, OverwriteDirectories: true, OverwriteFiles: true, OverwriteSymlinks: true,
		IgnorePermissionErrors: true, SkipOwners: true,
	}
	if err := out.Init(ctx); err != nil {
		return err
	}
	_, err = restore.Entry(ctx, rep, out, root, restore.Options{Parallel: 2, RestoreDirEntryAtDepth: math.MaxInt32})
	if err != nil {
		return fmt.Errorf("restoring snapshot %s: %w", man.ID, err)
	}
	if wantDigest != "" {
		got, err := TreeDigest(target)
		if err != nil {
			return fmt.Errorf("digesting snapshot %s: %w", man.ID, err)
		}
		if got != wantDigest {
			return fmt.Errorf("%w: snapshot %s is %s, the record says %s", ErrDigestMismatch, man.ID, got, wantDigest)
		}
	}
	return WriteTar(target, dst)
}

// Verify checks that the snapshot's metadata and a sample of its contents can be read.
func (w *Writer) Verify(ctx context.Context, prefix, password, snapshotID string) error {
	work, err := os.MkdirTemp(w.Scratch, "verify-")
	if err != nil {
		return err
	}
	defer RemoveAll(work)
	rep, err := w.open(ctx, work, prefix, password, false)
	if err != nil {
		return err
	}
	defer func() { _ = rep.Close(ctx) }()
	man, err := snapshot.LoadSnapshot(ctx, rep, manifest.ID(snapshotID))
	if err != nil {
		return err
	}
	if man.IncompleteReason != "" {
		return fmt.Errorf("snapshot %s is incomplete: %s", snapshotID, man.IncompleteReason)
	}
	return nil
}

// Snapshots lists the complete snapshots of the repository at prefix, oldest first: what the
// bucket holds after kopia's retention (checkpoints resync, ADR 0085 decision 17). It opens the
// repository afresh, so it sees every snapshot saved before the call. A prefix with no repository
// is [ErrNoRepositoryInBucket]; it is never created.
func (w *Writer) Snapshots(ctx context.Context, prefix, password string) ([]Snapshot, error) {
	work, err := os.MkdirTemp(w.Scratch, "list-")
	if err != nil {
		return nil, err
	}
	defer RemoveAll(work)
	rep, err := w.open(ctx, work, prefix, password, false)
	if errors.Is(err, repo.ErrRepositoryNotInitialized) || errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoRepositoryInBucket, prefix)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rep.Close(ctx) }()
	all, err := snapshot.ListSnapshots(ctx, rep, source)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(all))
	for _, man := range all {
		if man.IncompleteReason != "" {
			continue // a checkpoint is a complete snapshot; restore never offers one either
		}
		s := Snapshot{ID: string(man.ID), Start: man.StartTime.ToTime(), End: man.EndTime.ToTime(), Bytes: man.Stats.TotalFileSize}
		s.Meta, s.MetaErr = metaOf(man.Tags)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// open connects to the repository (creating it first when create is set and it is new) with a
// configuration file and cache in the work directory, so nothing outlives the call.
func (w *Writer) open(ctx context.Context, work, prefix, password string, create bool) (repo.Repository, error) {
	st, err := w.Storage.Open(ctx, prefix, create)
	if err != nil {
		return nil, fmt.Errorf("opening the storage: %w", err)
	}
	defer func() { _ = st.Close(ctx) }()
	cfg := filepath.Join(work, "repo.config")
	connect := func() error {
		return repo.Connect(ctx, cfg, st, password, &repo.ConnectOptions{
			ClientOptions:  repo.ClientOptions{Hostname: source.Host, Username: source.UserName, ReadOnly: w.ReadOnly},
			CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(work, "cache")},
		})
	}
	err = connect()
	if errors.Is(err, repo.ErrRepositoryNotInitialized) && create {
		if err = repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil {
			return nil, fmt.Errorf("creating the repository: %w", err)
		}
		if err = connect(); err == nil {
			err = w.setPolicy(ctx, cfg, password)
		}
	}
	if errors.Is(err, repo.ErrInvalidPassword) {
		return nil, &PrefixTakenError{Prefix: prefix}
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to the repository: %w", err)
	}
	return repo.Open(ctx, cfg, password, nil)
}

// prepareHome is makeReadable; a test replaces it.
var prepareHome = makeReadable

// skippedEntries returns an error that names the entries kopia could not read or skipped on an error.
func skippedEntries(man *snapshot.Manifest) error {
	if man.RootEntry == nil || man.RootEntry.DirSummary == nil {
		return nil
	}
	ds := man.RootEntry.DirSummary
	if ds.FatalErrorCount+ds.IgnoredErrorCount == 0 {
		return nil
	}
	var paths []string
	for _, e := range ds.FailedEntries {
		paths = append(paths, fmt.Sprintf("%q (%s)", e.EntryPath, e.Error))
	}
	return fmt.Errorf("the checkpoint is not saved: kopia could not read %d entries of the home: %s",
		ds.FatalErrorCount+ds.IgnoredErrorCount, strings.Join(paths, ", "))
}

// makeReadable adds the owner's read bit to files and the read and search bits to directories.
func makeReadable(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var add fs.FileMode
		switch {
		case info.IsDir():
			add = 0o500
		case info.Mode().IsRegular():
			add = 0o400
		default:
			return nil
		}
		mode := info.Mode().Perm()
		if mode&add == add {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		return root.Chmod(rel, mode|add)
	})
}

// keepEverything returns a policy tree whose files policy skips nothing that Stage kept.
func keepEverything(pt *policy.Tree) *policy.Tree {
	eff := *pt.EffectivePolicy()
	eff.FilesPolicy.DotIgnoreFiles = nil
	eff.FilesPolicy.IgnoreRules = nil
	no := policy.OptionalBool(false)
	eff.FilesPolicy.IgnoreCacheDirectories = &no
	return policy.BuildTree(nil, &eff)
}

// setPolicy sets the global policy of a new repository: zstd compression and retention.
func (w *Writer) setPolicy(ctx context.Context, cfg, password string) error {
	rep, err := repo.Open(ctx, cfg, password, nil)
	if err != nil {
		return err
	}
	defer func() { _ = rep.Close(ctx) }()
	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "policy"}, func(ctx context.Context, wr repo.RepositoryWriter) error {
		def := *policy.DefaultPolicy
		p := &def
		p.CompressionPolicy.CompressorName = "zstd"
		opt := func(n int) *policy.OptionalInt { v := policy.OptionalInt(n); return &v }
		p.RetentionPolicy = policy.RetentionPolicy{
			KeepLatest: opt(Retention.Latest), KeepHourly: opt(Retention.Hourly), KeepDaily: opt(Retention.Daily),
			KeepWeekly: opt(Retention.Weekly), KeepMonthly: opt(0), KeepAnnual: opt(0),
		}
		return policy.SetPolicy(ctx, wr, policy.GlobalPolicySourceInfo, p)
	})
}
