// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
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
}

// Meta is stamped on a snapshot as tags.
type Meta struct {
	CheckpointID string
	Generation   string
	Epoch        int64
}

// Result is a finished snapshot.
type Result struct {
	SnapshotID string
	Bytes      int64
	Files      int64
	Skipped    int64
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
		prev, err := snapshot.ListSnapshots(ctx, wr, source)
		if err != nil {
			return err
		}
		u := upload.NewUploader(wr)
		u.ParallelUploads = 2
		man, err := u.Upload(ctx, src, pt, source, prev...)
		if err != nil {
			return err
		}
		if man.IncompleteReason != "" {
			return fmt.Errorf("the snapshot is incomplete: %s", man.IncompleteReason)
		}
		man.Tags = map[string]string{"checkpoint": m.CheckpointID, "generation": m.Generation, "epoch": fmt.Sprint(m.Epoch)}
		if id, err = snapshot.SaveSnapshot(ctx, wr, man); err != nil {
			return err
		}
		_, err = policy.ApplyRetentionPolicy(ctx, wr, source, true)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return Result{SnapshotID: string(id), Bytes: stats.Bytes, Files: stats.Files, Skipped: stats.Skipped}, nil
}

// Restore writes the snapshot as a tar stream to out. A snapshot ID of "" restores the newest.
func (w *Writer) Restore(ctx context.Context, prefix, password, snapshotID string, dst io.Writer) error {
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
			ClientOptions:  repo.ClientOptions{Hostname: source.Host, Username: source.UserName},
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
	if err != nil {
		return nil, fmt.Errorf("connecting to the repository: %w", err)
	}
	return repo.Open(ctx, cfg, password, nil)
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
