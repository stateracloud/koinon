// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
)

// These stay plain tests, not Ginkgo specs: the TestAudit suite starts Postgres in BeforeSuite, so
// specs would skip without Docker, and these need no database and must run without it.

// scriptedRow answers InstallWriterCheck's query from a script, with no database: "ok" is a member,
// "error" a failed query, "stall" a query that returns only when its context ends.
type scriptedRow struct {
	script []string
	ctx    context.Context
	status string
}

func (s *scriptedRow) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	s.status, s.script = s.script[0], s.script[1:]
	s.ctx = ctx
	return s
}

func (s *scriptedRow) Scan(dest ...any) error {
	switch s.status {
	case "stall":
		<-s.ctx.Done()
		return s.ctx.Err()
	case "error":
		return errors.New("connection refused")
	}
	*dest[0].(*string) = "koinon-api"
	*dest[1].(*bool) = true
	*dest[2].(*bool) = true
	return nil
}

// A stalled database must not block the startup check or the loop: each check has its own timeout.
func TestInstallWriterCheckTimesOut(t *testing.T) {
	g := NewWithT(t)
	var logs bytes.Buffer
	c := &audit.InstallWriterCheck{
		DB: &scriptedRow{script: []string{"stall"}}, Log: slog.New(slog.NewTextHandler(&logs, nil)), Timeout: 20 * time.Millisecond,
	}
	done := make(chan audit.WriterStatus)
	go func() { done <- c.Check(context.Background()) }()
	select {
	case s := <-done:
		g.Expect(s).To(Equal(audit.WriterStatus{}))
	case <-time.After(5 * time.Second):
		t.Fatal("the check did not stop at its timeout")
	}
	g.Expect(logs.String()).To(ContainSubstring("level=WARN"))
}

// ok, error, ok is no new membership: "is now a member" is for a login that was not one.
func TestInstallWriterCheckErrorIsNotAChange(t *testing.T) {
	g := NewWithT(t)
	var logs bytes.Buffer
	c := &audit.InstallWriterCheck{
		DB: &scriptedRow{script: []string{"ok", "error", "error", "ok"}}, Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	c.Check(context.Background())
	g.Expect(logs.String()).To(ContainSubstring("can write the install audit chain"))
	logs.Reset()
	c.Check(context.Background())
	c.Check(context.Background())
	g.Expect(bytes.Count(logs.Bytes(), []byte("level=WARN"))).To(Equal(1), "one warning for a run of errors")
	logs.Reset()
	c.Check(context.Background())
	g.Expect(logs.String()).To(BeEmpty(), "back to ok, as before")
}
