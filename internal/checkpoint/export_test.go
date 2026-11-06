// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"

	"github.com/kopia/kopia/repo/blob"
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
