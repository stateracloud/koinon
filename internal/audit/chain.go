// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

// The audit log is one hash chain per org (ADR 0023, amended 2026-10-26). The database computes
// the chain in a BEFORE INSERT trigger; this file recomputes it in Go, independently of the SQL
// function audit_event_hash, to find the first row that no longer fits.
//
// hash = sha256(canonical_row || prev_hash). canonical_row is the concatenation of these fields,
// each as an 8-byte big-endian byte length followed by its UTF-8 bytes: the tag "koinon.audit.v1",
// org_id and id (lower-case UUID text), chain_seq (decimal), actor_kind, actor_id, action,
// target_kind, target_id, summary (jsonb text as Postgres prints it), request_id and created_at
// (decimal microseconds since the Unix epoch). prev_hash is appended as its 32 raw bytes; the first
// row of a chain has 32 zero bytes. A row's chain_seq counts from 1 without gaps.

const hashTag = "koinon.audit.v1"

// ZeroHash is the prev_hash of the first row of a chain.
var ZeroHash = make([]byte, sha256.Size)

func appendField(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

// RowHash computes the hash a chain row must have.
func RowHash(r store.ListAuditChainRowsRow) []byte {
	b := appendField(nil, hashTag)
	for _, f := range []string{
		r.OrgText, r.IDText, strconv.FormatInt(r.ChainSeq, 10), r.ActorKind, r.ActorID, r.Action,
		r.TargetKind, r.TargetID, r.SummaryText, r.RequestID, strconv.FormatInt(r.CreatedMicros, 10),
	} {
		b = appendField(b, f)
	}
	b = append(b, r.PrevHash...)
	sum := sha256.Sum256(b)
	return sum[:]
}

// Break is the first place an org's chain does not hold.
type Break struct {
	// Seq is the chain_seq of the row that does not fit; for a head that does not match the last row
	// it is the sequence after the last row.
	Seq    int64
	Reason string
}

// OrgResult is the verdict on one org's chain.
type OrgResult struct {
	Org  pgtype.UUID
	Rows int64
	// Break is nil when the chain holds.
	Break *Break
}

// OK reports whether the chain holds.
func (r OrgResult) OK() bool { return r.Break == nil }

const verifyPage = 1000

// VerifyOrg walks one org's chain in order and reports the first broken link. It checks that
// chain_seq counts from 1 without gaps, that each prev_hash is the previous row's hash, that each
// hash matches the row, and that the chain head names the last row (so removed tail rows show).
func VerifyOrg(ctx context.Context, q *store.Queries, org pgtype.UUID) (OrgResult, error) {
	res := OrgResult{Org: org}
	var (
		last int64
		prev = ZeroHash
	)
	for {
		rows, err := q.ListAuditChainRows(ctx, store.ListAuditChainRowsParams{OrgID: org, AfterSeq: last, PageSize: verifyPage})
		if err != nil {
			return res, fmt.Errorf("audit verify: read org %s: %w", org.String(), err)
		}
		for _, r := range rows {
			switch {
			case r.ChainSeq != last+1:
				res.Break = &Break{Seq: r.ChainSeq, Reason: fmt.Sprintf("chain_seq %d follows %d: a row is missing or was inserted", r.ChainSeq, last)}
			case !bytes.Equal(r.PrevHash, prev):
				res.Break = &Break{Seq: r.ChainSeq, Reason: "prev_hash is not the hash of the previous row"}
			case !bytes.Equal(r.Hash, RowHash(r)):
				res.Break = &Break{Seq: r.ChainSeq, Reason: fmt.Sprintf("hash does not match the content of row %s", r.IDText)}
			}
			if res.Break != nil {
				return res, nil
			}
			last, prev = r.ChainSeq, r.Hash
			res.Rows++
		}
		if len(rows) < verifyPage {
			break
		}
	}
	head, err := q.GetAuditChainHead(ctx, org)
	switch {
	case err != nil && last == 0:
		return res, nil // no rows and no head
	case err != nil:
		res.Break = &Break{Seq: last + 1, Reason: "the chain head is missing"}
	case head.Seq != last || !bytes.Equal(head.Hash, prev):
		res.Break = &Break{Seq: last + 1, Reason: fmt.Sprintf("the chain head says %d rows, the log has %d: rows were removed from the end or the head was changed", head.Seq, last)}
	}
	return res, nil
}

// Verify checks the chain of org, or of every org when org is not valid.
func Verify(ctx context.Context, q *store.Queries, org pgtype.UUID) ([]OrgResult, error) {
	orgs := []pgtype.UUID{org}
	if !org.Valid {
		var err error
		if orgs, err = q.ListAuditChainOrgs(ctx); err != nil {
			return nil, fmt.Errorf("audit verify: list orgs: %w", err)
		}
	}
	out := make([]OrgResult, 0, len(orgs))
	for _, o := range orgs {
		r, err := VerifyOrg(ctx, q, o)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ErrBroken is returned by the verifier command when a chain does not hold.
var ErrBroken = errors.New("audit chain is broken")

// WriteReport prints one line per org and reports whether every chain holds.
func WriteReport(w io.Writer, results []OrgResult) bool {
	ok := true
	for _, r := range results {
		if r.OK() {
			_, _ = fmt.Fprintf(w, "ok      org %s: %d events\n", r.Org.String(), r.Rows)
			continue
		}
		ok = false
		_, _ = fmt.Fprintf(w, "BROKEN  org %s: first broken link at chain_seq %d: %s\n", r.Org.String(), r.Break.Seq, r.Break.Reason)
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(w, "no audit events")
	}
	return ok
}
