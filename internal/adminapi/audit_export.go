// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
)

// ExportAudit implements StrictServerInterface: GET /audit/export (ADR 0100 decision 5). The scope
// rules are those of ListAuditEvents: org scope is the token's own org and never another; install
// scope needs a token whose root user is a platform admin (installAuditDenied, the check the install
// read uses). The authorization runs before the first byte; the chain is then streamed through a pipe
// (audit.Export pages through it), so a long chain is never held in memory.
func (s *Server) ExportAudit(ctx context.Context, req ExportAuditRequestObject) (ExportAuditResponseObject, error) {
	p, _ := s.actor(ctx)
	fail := func(err error) (ExportAuditResponseObject, error) {
		st, e := s.problem(err)
		return ExportAuditdefaultJSONResponse{Body: e, StatusCode: st}, nil
	}
	if sc := req.Params.Scope; sc != nil && !sc.Valid() {
		return fail(fmt.Errorf("%w: scope must be org or install", audit.ErrBadFilter))
	}
	var fromSeq int64
	if req.Params.FromSeq != nil {
		if fromSeq = *req.Params.FromSeq; fromSeq < 1 {
			return fail(fmt.Errorf("%w: fromSeq must be 1 or more", audit.ErrBadFilter))
		}
	}
	var sc audit.ExportScope
	if req.Params.Scope != nil && *req.Params.Scope == ExportAuditParamsScopeInstall {
		if req.Params.Org != nil {
			return fail(fmt.Errorf("%w: the install chain has no org", audit.ErrBadFilter))
		}
		denied, err := s.installAuditDenied(ctx, p)
		if err != nil {
			return fail(err)
		}
		if denied != nil {
			return ExportAuditdefaultJSONResponse{Body: *denied, StatusCode: http.StatusForbidden}, nil
		}
		sc.Install = true
	} else {
		if !p.Org.Valid {
			return ExportAuditdefaultJSONResponse{Body: *forbidden("this token has no org"), StatusCode: http.StatusForbidden}, nil
		}
		if o := req.Params.Org; o != nil && (pgtype.UUID{Bytes: *o, Valid: true}) != p.Org {
			return ExportAuditdefaultJSONResponse{Body: *forbidden("a token exports its own org's audit log only"), StatusCode: http.StatusForbidden}, nil
		}
		sc.Org = p.Org
	}
	export := s.exportAudit
	if export == nil {
		export = func(ctx context.Context, w io.Writer, sc audit.ExportScope, fromSeq int64) error {
			return audit.Export(ctx, store.New(s.pool), w, sc, fromSeq)
		}
	}
	pr, pw := io.Pipe()
	go func() {
		if s.exportDone != nil {
			defer s.exportDone()
		}
		err := export(ctx, pw, sc, fromSeq)
		// A closed pipe or a cancelled request means the client went away: the response writer's
		// own error says so, once (the response error handler logs it).
		if err != nil && !errors.Is(err, io.ErrClosedPipe) && ctx.Err() == nil {
			s.log.Error("audit export failed", "err", err)
		}
		_ = pw.CloseWithError(err)
	}()
	// Wait for the first bytes: a failure before the header is written (a fromSeq beyond the chain,
	// an unreadable head) is still an ordinary error response, not a 200 with an empty body. The
	// buffer is bufio's default size (4 KiB); it only holds what the first Peek read.
	body := &exportBody{r: bufio.NewReader(pr), pr: pr}
	if _, err := body.r.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		_ = pr.Close()
		return fail(err)
	}
	return ExportAudit200ApplicationxNdjsonResponse{Body: body}, nil
}

// exportBody is the export's response body. Close closes the pipe's reader, so the export
// goroutine's next write fails and it ends when the response ends early (the client went away):
// the generated visitor closes a body that is an io.Closer. A read error other than io.EOF is the
// export failing after the response started; it is returned as errStreamFailed, and the response
// error handler aborts the connection rather than end a truncated body cleanly. It deliberately has
// no WriteTo (as a bare *bufio.Reader would), so io.Copy cannot bypass that mapping.
type exportBody struct {
	r  *bufio.Reader
	pr *io.PipeReader
}

func (b *exportBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%w: %w", errStreamFailed, err)
	}
	return n, err
}

func (b *exportBody) Close() error { return b.pr.Close() }
