// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"context"
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
	pr, pw := io.Pipe()
	go func() {
		// A failure after the first byte ends the stream early: the file then fails verification
		// (its last line is not the header's head), which is the signal the reader gets.
		err := audit.Export(ctx, store.New(s.pool), pw, sc, fromSeq)
		if err != nil {
			s.log.Error("audit export failed", "err", err)
		}
		_ = pw.CloseWithError(err)
	}()
	return ExportAudit200ApplicationxNdjsonResponse{Body: pr}, nil
}
