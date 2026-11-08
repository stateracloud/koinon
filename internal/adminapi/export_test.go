// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
	"io"

	"github.com/stateracloud/koinon/internal/audit"
)

// SetAuditExporter replaces what GET /audit/export streams (audit.Export) and sets a hook that
// runs as the export goroutine ends. Tests only; call it before the first request.
func SetAuditExporter(s *Server, export func(ctx context.Context, w io.Writer, sc audit.ExportScope, fromSeq int64) error, done func()) {
	s.exportAudit, s.exportDone = export, done
}
