// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrDigestMismatch is returned when bytes do not have the pinned SHA-256: a download from the mirror, or an
// object that is already in the cluster store under the key of the pinned digest. The installer never
// overwrites such an object. An operator who knows the object is wrong deletes it and runs the installer again.
var ErrDigestMismatch = errors.New("gvisor asset does not have the pinned SHA-256")

// gvisorAssetPrefix is the key prefix of staged gVisor tarballs in the snapshot bucket. It has no "atespaces"
// segment, so the object-store plugin of atelet serves it (assets must not live under a snapshot location).
// It follows the layout of ate-setup for the microVM assets: <prefix>/<arch>/<sha256><suffix>.
const gvisorAssetPrefix = "gvisor-assets"

// archiveSuffixes are the archive formats atelet knows, with the suffix it needs in the URL. atelet chooses the
// decompressor from the suffix of the asset URL only, and the suffix must match the real compression of the
// bytes (issue #1157). So the suffix of a staged key comes from the bytes, never from a constant. Each suffix is
// the one atelet accepts first for its format.
var archiveSuffixes = []string{".tar.gz", ".tar.bz2", ".tar.zst", ".tar"}

// ErrUnknownArchive is returned when the bytes of a tarball are not gzip, bzip2, zstd or plain tar.
var ErrUnknownArchive = errors.New("gvisor asset is not a gzip, bzip2, zstd or tar archive")

// archiveSuffix detects the format of an archive from the magic number at the start of head (at least the first
// 262 bytes of the archive) and returns the suffix of its URL.
func archiveSuffix(head []byte) (string, error) {
	switch {
	// The strong check goes first: the name of the first member of a plain tar can start with the bytes of
	// another magic number.
	case len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return ".tar", nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return ".tar.gz", nil
	case bytes.HasPrefix(head, []byte("BZh")):
		return ".tar.bz2", nil
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return ".tar.zst", nil
	}
	return "", ErrUnknownArchive
}

// maxGVisorBytes bounds a download: the tarballs are about 60 MB.
const maxGVisorBytes = 1 << 30

// gvisorDownloadTimeout bounds the download and the upload of one tarball.
const gvisorDownloadTimeout = 20 * time.Minute

// AssetStore is the cluster object store, seen as the installer needs it. The installer owns the
// credentials: they come from the cluster and never reach a sandbox.
type AssetStore interface {
	// Digest returns the hex SHA-256 of the object, and false when there is no such object.
	Digest(ctx context.Context, key string) (sum string, found bool, err error)
	// Put writes size bytes of r as the object.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
}

// AssetFetcher opens the source of a tarball: the public gVisor bucket or an operator mirror.
type AssetFetcher interface {
	Open(ctx context.Context, url string) (io.ReadCloser, error)
}

// StagedAsset is one tarball in the cluster store.
type StagedAsset struct {
	Arch   string // GOARCH: amd64 or arm64
	Key    string // the object key in the bucket
	SHA256 string
}

// StageResult says what StageGVisor did for each architecture.
type StageResult struct {
	Assets []StagedAsset
	// Uploaded and Present are the architectures that were written and the ones already in the store.
	Uploaded, Present []string
}

// gvisorKey is the object key of an architecture for an archive suffix: <prefix>/<arch>/<sha256><suffix>.
func gvisorKey(a gvisorArch, suffix string) string {
	return gvisorAssetPrefix + "/" + a.goArch + "/" + a.sha256 + suffix
}

// findStagedGVisor looks for the object of the pinned digest under each archive suffix. It returns the key of
// the one that has the pinned digest. An object under any such key with another digest is an error
// (ErrDigestMismatch), never overwritten. A key without a suffix (staged before issue #1157) is not looked at.
func findStagedGVisor(ctx context.Context, store AssetStore, a gvisorArch) (key string, found bool, err error) {
	for _, suffix := range archiveSuffixes {
		k := gvisorKey(a, suffix)
		sum, ok, err := store.Digest(ctx, k)
		if err != nil {
			return "", false, fmt.Errorf("read %s from the cluster object store: %w", k, err)
		}
		switch {
		case !ok:
		case sum == a.sha256:
			return k, true, nil
		default:
			return "", false, fmt.Errorf("%w: the object %s in the cluster object store has SHA-256 %s, not %s. The installer does not overwrite it. Delete the object if it is wrong, then run the installer again", ErrDigestMismatch, k, sum, a.sha256)
		}
	}
	return "", false, nil
}

// gvisorSourceURL is the URL of the tarball of an architecture below the mirror base (empty is the public bucket).
func gvisorSourceURL(mirror string, a gvisorArch) string {
	if mirror == "" {
		mirror = GVisorDefaultMirror
	}
	return strings.TrimSuffix(mirror, "/") + "/" + GVisorRelease + "/" + a.dir + "/gvisor.tar.zstd"
}

// StageGVisor makes sure the cluster store holds the pinned gVisor tarball of every architecture, and
// returns where. It is idempotent: an object that has the pinned digest is left alone and nothing is
// fetched. An object with another digest is an error (ErrDigestMismatch), never overwritten. A download is
// checked against the pinned digest before the first byte goes to the store, and the stored object is
// read back and checked once more.
func StageGVisor(ctx context.Context, store AssetStore, fetch AssetFetcher, mirror string, out io.Writer) (StageResult, error) {
	return StageGVisorWithin(ctx, ctx, store, fetch, mirror, out)
}

// StageGVisorWithin is StageGVisor with a context for the bucket readiness request. The requests for the first
// architecture, which find out whether the bucket exists, use probe: a deadline on probe stops one stalled request
// from running past the wait for the bucket. Every other request, and the tarball download with its own
// timeout, uses ctx.
func StageGVisorWithin(ctx, probe context.Context, store AssetStore, fetch AssetFetcher, mirror string, out io.Writer) (StageResult, error) {
	var res StageResult
	for _, a := range gvisorArches {
		key, found, err := findStagedGVisor(probe, store, a)
		if err != nil {
			return res, err
		}
		probe = ctx // the bucket answered for the first architecture: it exists
		if found {
			_, _ = fmt.Fprintf(out, "gvisor %s %s: already in the cluster object store (%s)\n", GVisorRelease, a.goArch, key)
			res.Present = append(res.Present, a.goArch)
		} else {
			key, err = uploadGVisor(ctx, store, fetch, gvisorSourceURL(mirror, a), a)
			if err != nil {
				return res, err
			}
			_, _ = fmt.Fprintf(out, "gvisor %s %s: staged in the cluster object store (%s)\n", GVisorRelease, a.goArch, key)
			res.Uploaded = append(res.Uploaded, a.goArch)
		}
		res.Assets = append(res.Assets, StagedAsset{Arch: a.goArch, Key: key, SHA256: a.sha256})
	}
	return res, nil
}

func uploadGVisor(ctx context.Context, store AssetStore, fetch AssetFetcher, url string, a gvisorArch) (string, error) {
	// A stalled mirror must not hang the installer: one download gets a fixed time.
	ctx, cancel := context.WithTimeout(ctx, gvisorDownloadTimeout)
	defer cancel()
	src, err := fetch.Open(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetch the gvisor tarball for %s from %s: %w. Use --gvisor-mirror for a mirror the cluster host can reach", a.goArch, redactURL(url), err)
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp("", "koinon-gvisor-*.tar")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()); _ = tmp.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, maxGVisorBytes+1))
	if err != nil {
		return "", fmt.Errorf("download the gvisor tarball for %s from %s: %w", a.goArch, redactURL(url), err)
	}
	if n > maxGVisorBytes {
		return "", fmt.Errorf("the gvisor tarball for %s from %s is larger than %d bytes", a.goArch, redactURL(url), int64(maxGVisorBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.sha256 {
		return "", fmt.Errorf("%w: %s has SHA-256 %s, not %s. Nothing was written to the cluster object store", ErrDigestMismatch, redactURL(url), got, a.sha256)
	}
	head := make([]byte, 262)
	hn, err := tmp.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	suffix, err := archiveSuffix(head[:hn])
	if err != nil {
		return "", fmt.Errorf("%w: %s. Nothing was written to the cluster object store", err, redactURL(url))
	}
	key := gvisorKey(a, suffix)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err := store.Put(ctx, key, tmp, n); err != nil {
		return "", fmt.Errorf("write %s to the cluster object store: %w", key, err)
	}
	sum, found, err := store.Digest(ctx, key)
	switch {
	case err != nil:
		return "", fmt.Errorf("read back %s from the cluster object store: %w", key, err)
	case !found:
		return "", fmt.Errorf("the object %s is not in the cluster object store after the write", key)
	case sum != a.sha256:
		return "", fmt.Errorf("%w: the object %s has SHA-256 %s after the write, not %s", ErrDigestMismatch, key, sum, a.sha256)
	}
	return key, nil
}

// redactURL drops user info, query and fragment: a mirror URL can carry a token.
func redactURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if at, scheme := strings.LastIndex(raw, "@"), strings.Index(raw, "://"); at > 0 && scheme >= 0 && at > scheme {
		raw = raw[:scheme+3] + raw[at+1:]
	}
	return raw
}

// EnsureGVisorAssets stages the gVisor tarball and applies the s3:// SandboxConfig for it. It runs on the
// platforms with an in-cluster object store (kind and generic) when the install has the gvisor class. On gke
// the gs://gvisor default stays: that route has no in-cluster store.
func (i *Installer) EnsureGVisorAssets(ctx context.Context) error {
	if i.Profile.Platform == PlatformGKE || !i.Profile.UsesGVisor() {
		return nil
	}
	open := i.GVisorStore
	if open == nil {
		open = i.openClusterStore
	}
	store, closeStore, err := open(ctx)
	if err != nil {
		return fmt.Errorf("stage gvisor: %w", err)
	}
	defer closeStore()
	fetch := i.GVisorFetcher
	if fetch == nil {
		fetch = NewHTTPFetcher()
	}
	// The bucket-init Job of the object store may still run when the installer gets here (ate-setup does not
	// wait for it): retry while the bucket is missing, up to the wait timeout.
	waitCtx, cancel := context.WithTimeout(ctx, orDuration(i.WaitTimeout, 2*time.Minute))
	defer cancel()
	var res StageResult
	for {
		res, err = StageGVisorWithin(ctx, waitCtx, store, fetch, i.Profile.GVisorMirror, i.Out)
		if err == nil || !errors.Is(err, ErrNoBucket) || waitCtx.Err() != nil {
			break
		}
		_, _ = fmt.Fprintln(i.Out, "waiting for the bucket of the cluster object store (its bucket-init Job is still running)")
		select {
		case <-waitCtx.Done():
		case <-time.After(orDuration(i.PollInterval, 2*time.Second)):
		}
	}
	if err != nil {
		return fmt.Errorf("stage gvisor: %w", err)
	}
	changed, err := ApplySandboxConfig(ctx, i.Dynamic, i.Profile.StorageBucket, res.Assets)
	if err != nil {
		return fmt.Errorf("stage gvisor: %w", err)
	}
	if changed {
		_, _ = fmt.Fprintf(i.Out, "SandboxConfig %s: applied with the s3:// tarballs of gvisor %s\n", GVisorSandboxConfigName, GVisorRelease)
	}
	if warn := gvisorOrderWarning(ctx, i.Dynamic); warn != "" {
		_, _ = fmt.Fprintln(i.Out, warn)
	}
	return nil
}
