// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/kopia/kopia/repo/blob"
)

// kopiaBlobPrefixes are the blob ID prefixes kopia writes (kopia v0.23: content.PackBlobIDPrefixRegular
// p and PackBlobIDPrefixSpecial q, content.BlobIDPrefixSession s, indexblob's V0 n, m and l, the
// epoch manager's x, the log blobs _log_, and kopia.repository, kopia.blobcfg and their backups).
// None of them is r followed by a digit, the name of a later epoch's folder (EpochPrefix): listing
// only these is what keeps the sweep out of a later epoch. A spec lists every blob a repository
// holds and fails on one outside this list, so a kopia version that adds a prefix shows up there
// rather than as storage the sweep never frees.
var kopiaBlobPrefixes = []blob.ID{"p", "q", "s", "n", "m", "l", "x", "_log_", "kopia."}

// laterEpoch matches a blob ID inside a later repository epoch's folder. With the prefix list above
// it never matches; it is a second belt, should a listed prefix ever start with r.
var laterEpoch = regexp.MustCompile(`^r[0-9]`)

// DeleteRepository deletes the blobs of the repository at prefix and returns how many it deleted.
// It lists only kopia's own prefixes, so a later epoch's repository under prefix (r<N>/) is never
// touched; a repository that was never written has nothing to delete. It needs no password: it
// reads no blob. Deleting again after a failure picks up where it stopped.
func (w *Writer) DeleteRepository(ctx context.Context, prefix string) (int, error) {
	if w.ReadOnly {
		return 0, errors.New(ReadOnlyError)
	}
	if !strings.HasSuffix(prefix, "/") || !strings.HasPrefix(prefix, "orgs/") {
		return 0, fmt.Errorf("checkpoint: %q is not a repository prefix", prefix)
	}
	st, err := w.Storage.Open(ctx, prefix, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("opening the repository at %s: %w", prefix, err)
	}
	defer func() { _ = st.Close(context.WithoutCancel(ctx)) }()
	n := 0
	for _, p := range kopiaBlobPrefixes {
		var ids []blob.ID
		if err := st.ListBlobs(ctx, p, func(m blob.Metadata) error {
			if !laterEpoch.MatchString(string(m.BlobID)) {
				ids = append(ids, m.BlobID)
			}
			return nil
		}); err != nil {
			return n, fmt.Errorf("listing the blobs of %s: %w", prefix, err)
		}
		for _, id := range ids {
			if err := st.DeleteBlob(ctx, id); err != nil && !errors.Is(err, blob.ErrBlobNotFound) {
				return n, fmt.Errorf("deleting blob %s of %s: %w", id, prefix, err)
			}
			n++
		}
	}
	return n, nil
}
