// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/snapshot"
	. "github.com/onsi/ginkgo/v2"
)

// KopiaBlobPrefixes are the prefixes DeleteRepository lists.
var KopiaBlobPrefixes = kopiaBlobPrefixes

// BlobIDs lists every blob under prefix, whatever its name.
func (w *Writer) BlobIDs(ctx context.Context, prefix string) ([]blob.ID, error) {
	st, err := w.Storage.Open(ctx, prefix, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close(ctx) }()
	var ids []blob.ID
	err = st.ListBlobs(ctx, "", func(m blob.Metadata) error {
		ids = append(ids, m.BlobID)
		return nil
	})
	return ids, err
}

// OpenForTest opens the repository at prefix as the writer does and closes the storage with it.
func (w *Writer) OpenForTest(ctx context.Context, work, prefix, password string) (repo.Repository, error) {
	return w.open(ctx, work, prefix, password, false)
}

// SkipMakeReadable stops the writer from making the staged home readable, for the rest of the spec.
func SkipMakeReadable() {
	prev := prepareHome
	prepareHome = func(string) error { return nil }
	DeferCleanup(func() { prepareHome = prev })
}

// SkippedEntries is skippedEntries on a manifest whose root holds the given failed entries.
func SkippedEntries(fatal, ignored int, failed ...fs.EntryWithError) error {
	ds := &fs.DirectorySummary{FatalErrorCount: fatal, IgnoredErrorCount: ignored}
	for i := range failed {
		ds.FailedEntries = append(ds.FailedEntries, &failed[i])
	}
	return skippedEntries(&snapshot.Manifest{RootEntry: &snapshot.DirEntry{DirSummary: ds}})
}
