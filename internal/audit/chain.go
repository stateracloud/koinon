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

	"github.com/jackc/pgx/v5"
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

const (
	hashTag        = "koinon.audit.v1"
	installHashTag = "koinon.install_audit.v1"
)

// ZeroHash is the prev_hash of the first row of a chain.
var ZeroHash = make([]byte, sha256.Size)

func appendField(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

// chainFields are the values both kinds of chain hash: the scope ID (the org's ID, or the install's
// in its place, ADR 0100 decision 1.3) and the fields the two tables share.
type chainFields struct {
	Scope, ID                                        string
	Seq                                              int64
	ActorKind, ActorID, Action, TargetKind, TargetID string
	Summary, RequestID                               string
	CreatedMicros                                    int64
	PrevHash                                         []byte
}

// hashRow is the one encoder of both chains; only the tag and the scope ID differ.
func hashRow(tag string, f chainFields) []byte {
	b := appendField(nil, tag)
	for _, v := range []string{
		f.Scope, f.ID, strconv.FormatInt(f.Seq, 10), f.ActorKind, f.ActorID, f.Action,
		f.TargetKind, f.TargetID, f.Summary, f.RequestID, strconv.FormatInt(f.CreatedMicros, 10),
	} {
		b = appendField(b, v)
	}
	b = append(b, f.PrevHash...)
	sum := sha256.Sum256(b)
	return sum[:]
}

// RowHash computes the hash an org chain row must have.
func RowHash(r store.ListAuditChainRowsRow) []byte {
	return hashRow(hashTag, chainFields{
		Scope: r.OrgText, ID: r.IDText, Seq: r.ChainSeq, ActorKind: r.ActorKind, ActorID: r.ActorID, Action: r.Action,
		TargetKind: r.TargetKind, TargetID: r.TargetID, Summary: r.SummaryText, RequestID: r.RequestID,
		CreatedMicros: r.CreatedMicros, PrevHash: r.PrevHash,
	})
}

// InstallRowHash computes the hash an install chain row must have.
func InstallRowHash(r store.ListInstallAuditChainRowsRow) []byte {
	return hashRow(installHashTag, chainFields{
		Scope: r.ScopeText, ID: r.IDText, Seq: r.ChainSeq, ActorKind: r.ActorKind, ActorID: r.ActorID, Action: r.Action,
		TargetKind: r.TargetKind, TargetID: r.TargetID, Summary: r.SummaryText, RequestID: r.RequestID,
		CreatedMicros: r.CreatedMicros, PrevHash: r.PrevHash,
	})
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

// link is one row of a chain as the walker needs it: the stored values and the hash recomputed from
// the row's content.
type link struct {
	seq        int64
	id         string
	prev, hash []byte
	want       []byte
}

// walk reads a chain page by page in order and returns the rows seen, the break (nil when the chain
// holds), and the last chain_seq and hash. It checks that chain_seq counts from 1 without gaps, that
// each prev_hash is the previous row's hash and that each hash matches the row.
func walk(fetch func(afterSeq int64) ([]link, error)) (rows, last int64, prev []byte, brk *Break, err error) {
	prev = ZeroHash
	for {
		page, err := fetch(last)
		if err != nil {
			return rows, last, prev, nil, err
		}
		for _, r := range page {
			switch {
			case r.seq != last+1:
				brk = &Break{Seq: r.seq, Reason: fmt.Sprintf("chain_seq %d follows %d: a row is missing or was inserted", r.seq, last)}
			case !bytes.Equal(r.prev, prev):
				brk = &Break{Seq: r.seq, Reason: "prev_hash is not the hash of the previous row"}
			case !bytes.Equal(r.hash, r.want):
				brk = &Break{Seq: r.seq, Reason: fmt.Sprintf("hash does not match the content of row %s", r.id)}
			}
			if brk != nil {
				return rows, last, prev, brk, nil
			}
			last, prev = r.seq, r.hash
			rows++
		}
		if len(page) < verifyPage {
			return rows, last, prev, nil, nil
		}
	}
}

// headBreak compares a chain head with the last row: it finds removed tail rows and a changed head.
// found is false when there is no head row at all; seeded says the chain always has one (the
// install chain's, which migration 00066 creates), so a missing head is a break even with no rows.
// An org's head appears with its first event, so an org with neither has an empty, intact chain.
func headBreak(seeded, found bool, headSeq int64, headHash []byte, last int64, prev []byte) *Break {
	switch {
	case !found && last == 0 && !seeded:
		return nil // no rows and no head
	case !found:
		return &Break{Seq: last + 1, Reason: "the chain head is missing"}
	case headSeq != last || !bytes.Equal(headHash, prev):
		return &Break{Seq: last + 1, Reason: fmt.Sprintf("the chain head says %d rows, the log has %d: rows were removed from the end or the head was changed", headSeq, last)}
	}
	return nil
}

// VerifyOrg walks one org's chain in order and reports the first broken link. It checks that
// chain_seq counts from 1 without gaps, that each prev_hash is the previous row's hash, that each
// hash matches the row, and that the chain head names the last row (so removed tail rows show).
func VerifyOrg(ctx context.Context, q *store.Queries, org pgtype.UUID) (OrgResult, error) {
	res := OrgResult{Org: org}
	rows, last, prev, brk, err := walk(func(after int64) ([]link, error) {
		page, err := q.ListAuditChainRows(ctx, store.ListAuditChainRowsParams{OrgID: org, AfterSeq: after, PageSize: verifyPage})
		if err != nil {
			return nil, fmt.Errorf("audit verify: read org %s: %w", org.String(), err)
		}
		out := make([]link, len(page))
		for i, r := range page {
			out[i] = link{seq: r.ChainSeq, id: r.IDText, prev: r.PrevHash, hash: r.Hash, want: RowHash(r)}
		}
		return out, nil
	})
	res.Rows, res.Break = rows, brk
	if err != nil || brk != nil {
		return res, err
	}
	head, err := q.GetAuditChainHead(ctx, org)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("audit verify: read head of org %s: %w", org.String(), err)
	}
	res.Break = headBreak(false, err == nil, head.Seq, head.Hash, last, prev)
	return res, nil
}

// InstallResult is the verdict on the install chain.
type InstallResult struct {
	Rows int64
	// Break is nil when the chain holds.
	Break *Break
}

// OK reports whether the chain holds.
func (r InstallResult) OK() bool { return r.Break == nil }

// VerifyInstall walks the install chain as VerifyOrg walks an org's, with the hash of tag
// koinon.install_audit.v1. The install head is seeded by the migration, so a missing head is a
// break even before the first event.
func VerifyInstall(ctx context.Context, q *store.Queries) (InstallResult, error) {
	var res InstallResult
	rows, last, prev, brk, err := walk(func(after int64) ([]link, error) {
		page, err := q.ListInstallAuditChainRows(ctx, store.ListInstallAuditChainRowsParams{AfterSeq: after, PageSize: verifyPage})
		if err != nil {
			return nil, fmt.Errorf("audit verify: read the install chain: %w", err)
		}
		out := make([]link, len(page))
		for i, r := range page {
			out[i] = link{seq: r.ChainSeq, id: r.IDText, prev: r.PrevHash, hash: r.Hash, want: InstallRowHash(r)}
		}
		return out, nil
	})
	res.Rows, res.Break = rows, brk
	if err != nil || brk != nil {
		return res, err
	}
	head, err := q.GetInstallAuditChainHead(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("audit verify: read the install chain head: %w", err)
	}
	res.Break = headBreak(true, err == nil, head.Seq, head.Hash, last, prev)
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

// WriteInstallReport prints the verdict on the install chain and reports whether it holds.
func WriteInstallReport(w io.Writer, r InstallResult) bool {
	if r.OK() {
		_, _ = fmt.Fprintf(w, "ok      install chain: %d events\n", r.Rows)
		return true
	}
	_, _ = fmt.Fprintf(w, "BROKEN  install chain: first broken link at chain_seq %d: %s\n", r.Break.Seq, r.Break.Reason)
	return false
}
