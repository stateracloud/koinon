// SPDX-License-Identifier: Apache-2.0

package declarative

import (
	"context"
	"time"
)

// SetOrgBackoff sets the first retry delay for Secrets that wait for their org.
func (i *Importer) SetOrgBackoff(d time.Duration) { i.orgBackoff = d }

// SetSyncFn replaces the pass Run repeats; it reports whether a Secret waits for its org.
func (i *Importer) SetSyncFn(f func(context.Context) (bool, error)) { i.syncFn = f }

// SetAfter replaces the wait Run uses between org retries; f receives the requested duration.
func (i *Importer) SetAfter(f func(time.Duration) <-chan time.Time) { i.after = f }
