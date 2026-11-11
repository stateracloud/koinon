// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

var _ = Describe("koinon-api audit verify", func() {
	It("walks the install chain by default, alone with --install, and reports a break", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		for range 3 {
			_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
				b.Install(audit.InstallEvent{
					Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "cli",
				})
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}

		By("an intact chain: the install line comes first, both modes exit zero")
		var out bytes.Buffer
		Expect(verifyAudit(ctx, pool, "", false, &out)).To(Succeed())
		Expect(out.String()).To(HavePrefix("ok      install chain: 3 events"))
		out.Reset()
		Expect(verifyAudit(ctx, pool, "", true, &out)).To(Succeed())
		Expect(out.String()).To(Equal("ok      install chain: 3 events\n"))

		By("--install and --org together are refused")
		Expect(verifyAudit(ctx, pool, "00000000-0000-0000-0000-000000000001", true, &out)).To(MatchError(ContainSubstring("exclusive")))

		By("a tampered install row breaks the default run and --install, and an --org run does not look at it")
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		Expect(err).NotTo(HaveOccurred())
		// audit-lint:allow a tamper test: forges a row past the triggers so "audit verify" has a break to report
		_, err = tx.Exec(ctx, `UPDATE install_audit_events SET target_id = 'forged' WHERE chain_seq = 2`)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Commit(ctx)).To(Succeed())

		out.Reset()
		Expect(verifyAudit(ctx, pool, "", false, &out)).To(MatchError(audit.ErrBroken))
		Expect(out.String()).To(ContainSubstring("BROKEN  install chain: first broken link at chain_seq 2"))
		out.Reset()
		Expect(verifyAudit(ctx, pool, "", true, &out)).To(MatchError(audit.ErrBroken))
		Expect(out.String()).To(ContainSubstring("chain_seq 2"))
	}, SpecTimeout(3*time.Minute))
})

var _ = Describe("koinon-api audit export and verify --file", func() {
	It("exports the install chain, and verify --file checks it with no database; --after continues a partial export", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		write := func(n int) {
			for range n {
				_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
					b.Install(audit.InstallEvent{Actor: audit.System("koinon-test:spec"), Action: "platform_price.set", TargetKind: "platform_price", TargetID: "cli"})
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		export := func(name string, from int64) string {
			path := filepath.Join(GinkgoT().TempDir(), name)
			f, err := os.Create(path)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = f.Close() }()
			Expect(audit.Export(ctx, store.New(pool), f, audit.ExportScope{Install: true}, from)).To(Succeed())
			return path
		}
		write(3)
		first := export("first.jsonl", 1)
		write(2)
		rest := export("rest.jsonl", 4)

		run := func(args ...string) (string, error) {
			var out bytes.Buffer
			cmd := newAuditCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			return out.String(), err
		}

		By("no database URL is needed with --file")
		out, err := run("verify", "--file", first)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(HavePrefix("ok      install chain (file): 3 events"))
		Expect(out).To(ContainSubstring("note    a file verified alone proves internal consistency only, not authenticity; use --after, or --anchors with an install export (later checkpoints), to pin it\n"),
			"a file checked alone says what that does not prove")

		By("a partial export: unchecked alone, checked with --after")
		out, err = run("verify", "--file", rest)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("first prev_hash is not checked"))
		Expect(out).To(ContainSubstring("proves internal consistency only"))
		out, err = run("verify", "--file", rest, "--after", first)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).NotTo(ContainSubstring("not checked"))
		Expect(out).NotTo(ContainSubstring("proves internal consistency only"), "--after pins the file")

		By("an --after file that is itself partial is named in a note: the chain before it is not covered")
		write(1)
		tail := export("tail.jsonl", 6)
		out, err = run("verify", "--file", tail, "--after", rest)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("note    --after " + rest + " starts at chain_seq 4"))
		out, err = run("verify", "--file", rest, "--after", first)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).NotTo(ContainSubstring("--after " + first))

		By("--after - is refused: it must be a file path")
		out, err = run("verify", "--file", first, "--after", "-")
		Expect(err).To(MatchError(ContainSubstring("--after must be a file path, not stdin")), out)
		_, err = run("verify", "--file", "-", "--after", "-")
		Expect(err).To(MatchError(ContainSubstring("--after must be a file path, not stdin")))

		By("a tampered file exits non-zero with the first broken chain_seq")
		raw, err := os.ReadFile(first)
		Expect(err).NotTo(HaveOccurred())
		bad := filepath.Join(GinkgoT().TempDir(), "bad.jsonl")
		Expect(os.WriteFile(bad, bytes.Replace(raw, []byte("platform_price.set"), []byte("platform_price.xxx"), 3), 0o600)).To(Succeed())
		out, err = run("verify", "--file", bad)
		Expect(err).To(MatchError(audit.ErrBroken))
		Expect(out).To(ContainSubstring("BROKEN  install chain (file): first broken link at chain_seq 1"))
		Expect(out).To(ContainSubstring("proves internal consistency only"))

		By("export --out removes a partial file when the export fails, so the next run can create it")
		dsn := pool.Config().ConnString()
		outFile := filepath.Join(GinkgoT().TempDir(), "out.jsonl")
		_, err = run("export", "--database-url", dsn, "--scope", "install", "--from-seq", "1000000", "--out", outFile)
		Expect(err).To(HaveOccurred())
		Expect(outFile).NotTo(BeAnExistingFile(), "the failed export left no file")
		out, err = run("export", "--database-url", dsn, "--scope", "install", "--out", outFile)
		Expect(err).NotTo(HaveOccurred(), out)
		out, err = run("verify", "--file", outFile)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(HavePrefix("ok      install chain (file): 6 events"))
		By("an existing file is never overwritten, and a refused create leaves it in place")
		_, err = run("export", "--database-url", dsn, "--scope", "install", "--out", outFile)
		Expect(err).To(MatchError(ContainSubstring("file exists")))
		Expect(outFile).To(BeAnExistingFile())

		By("a database URL from the environment (the pod sets DATABASE_URL) does not stop --file")
		GinkgoT().Setenv("DATABASE_URL", "postgres://koinon_test_not-a-real-host/db")
		var rootOut bytes.Buffer
		rootCmd := NewRootCommand()
		rootCmd.SetOut(&rootOut)
		rootCmd.SetErr(&rootOut)
		rootCmd.SetArgs([]string{"audit", "verify", "--file", first})
		Expect(rootCmd.ExecuteContext(ctx)).To(Succeed(), rootOut.String())
		Expect(rootOut.String()).To(HavePrefix("ok      install chain (file): 3 events"))

		By("flag combinations that make no sense are refused")
		_, err = run("verify")
		Expect(err).To(MatchError(ContainSubstring("database-url")))
		_, err = run("verify", "--file", first, "--org", "00000000-0000-0000-0000-000000000001")
		Expect(err).To(MatchError(ContainSubstring("--file")))
		_, err = run("verify", "--after", first, "--database-url", "x")
		Expect(err).To(MatchError(ContainSubstring("--after needs --file")))
	}, SpecTimeout(3*time.Minute))

	It("audit anchor writes an anchor, and verify --anchors pins an org export to an install export", func(ctx SpecContext) {
		pool := pgtest.Start(ctx)
		org, _, _ := pgtest.NewOrg(ctx, pool)
		other, _, _ := pgtest.NewOrg(ctx, pool)
		for _, o := range []pgtype.UUID{org, other} {
			for range 3 {
				_, err := audit.InTx(ctx, pool, func(_ *store.Queries, b audit.Batch) error {
					b.Org(audit.Event{Org: o, Actor: audit.System("koinon-test:spec"), Action: "test.cli", TargetKind: "test", TargetID: "t"})
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		run := func(args ...string) (string, error) {
			var out bytes.Buffer
			cmd := newAuditCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			return out.String(), err
		}
		export := func(q *store.Queries, name string, scope audit.ExportScope) string {
			path := filepath.Join(GinkgoT().TempDir(), name)
			f, err := os.Create(path)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = f.Close() }()
			Expect(audit.Export(ctx, q, f, scope, 1)).To(Succeed())
			return path
		}

		out, err := run("anchor", "--database-url", pool.Config().ConnString())
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("anchored 2 org chains at install chain_seq"))

		out, err = run("verify", "--database-url", pool.Config().ConnString())
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("1 anchors checked against the org chains"))

		out, err = run("verify", "--org", org.String(), "--database-url", pool.Config().ConnString())
		Expect(err).NotTo(HaveOccurred(), out)

		q := store.New(pool)
		inst := export(q, "install.jsonl", audit.ExportScope{Install: true})
		for _, o := range []pgtype.UUID{org, other} {
			orgFile := export(q, "org-"+o.String()+".jsonl", audit.ExportScope{Org: o})
			out, err = run("verify", "--file", orgFile, "--anchors", inst)
			Expect(err).NotTo(HaveOccurred(), out)
			Expect(out).To(ContainSubstring("1 anchors hold in this chain"), "the anchor keeps this org's head, whichever org the file is of")
			Expect(out).NotTo(ContainSubstring("proves internal consistency only"), "--anchors pins the file")
		}
		orgFile := export(q, "org.jsonl", audit.ExportScope{Org: org})

		By("an org chain rewritten behind the anchor, self-consistent, fails on the anchor")
		tx, err := pool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = tx.Rollback(ctx) })
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		Expect(err).NotTo(HaveOccurred())
		var prev []byte
		Expect(tx.QueryRow(ctx, `SELECT hash FROM audit_events WHERE org_id = $1 AND chain_seq = 1`, org).Scan(&prev)).To(Succeed())
		for seq := int64(2); seq <= 3; seq++ {
			// audit-lint:allow a tamper test: rewrites an org chain past the triggers so the anchor has a break to find
			_, err = tx.Exec(ctx, `UPDATE audit_events SET actor_id = 'koinon-test:mallory', prev_hash = $3 WHERE org_id = $1 AND chain_seq = $2`, org, seq, prev)
			Expect(err).NotTo(HaveOccurred())
			// audit-lint:allow a tamper test: recomputes the rewritten row's hash
			Expect(tx.QueryRow(ctx, `UPDATE audit_events SET hash = audit_event_hash(audit_events) WHERE org_id = $1 AND chain_seq = $2 RETURNING hash`, org, seq).Scan(&prev)).To(Succeed())
		}
		// audit-lint:allow a tamper test: moves the head to the rewritten chain
		_, err = tx.Exec(ctx, `UPDATE audit_chain_heads SET hash = $2 WHERE org_id = $1`, org, prev)
		Expect(err).NotTo(HaveOccurred())
		bad := export(store.New(tx), "bad.jsonl", audit.ExportScope{Org: org})
		out, err = run("verify", "--file", bad)
		Expect(err).NotTo(HaveOccurred(), "the rewritten file agrees with itself: %s", out)
		out, err = run("verify", "--file", bad, "--anchors", inst)
		Expect(err).To(MatchError(audit.ErrBroken))
		Expect(out).To(ContainSubstring("rewritten behind the anchor"))

		By("--anchors needs --file, and an install export")
		_, err = run("verify", "--anchors", inst)
		Expect(err).To(MatchError(ContainSubstring("--anchors needs --file")))
		_, err = run("verify", "--file", inst, "--anchors", inst)
		Expect(err).To(MatchError(ContainSubstring("--anchors is for an org export")))
		_, err = run("verify", "--file", orgFile, "--anchors", orgFile)
		Expect(err).To(MatchError(ContainSubstring("is an org export; give an install export")))
		Expect(err.Error()).NotTo(ContainSubstring("CollectAnchors"), "operator sees a CLI message, not the library option name")
	}, SpecTimeout(3*time.Minute))

	It("reads --scope and --org", func() {
		sc, err := exportScope("install", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(sc.Install).To(BeTrue())
		_, err = exportScope("install", "00000000-0000-0000-0000-000000000001")
		Expect(err).To(HaveOccurred())
		sc, err = exportScope("org", "00000000-0000-0000-0000-000000000001")
		Expect(err).NotTo(HaveOccurred())
		Expect(sc.Org.Valid).To(BeTrue())
		for _, bad := range [][2]string{{"org", ""}, {"org", "nope"}, {"", ""}, {"all", ""}} {
			_, err = exportScope(bad[0], bad[1])
			Expect(err).To(HaveOccurred(), bad[0]+bad[1])
		}
	})
})

// probeRetry is an installRetry whose lines go to its own buffer, not the global slog default, so a retry
// goroutine that outlives its spec cannot write into another spec's logs. A spec that starts the retry waits
// for the "recorded after a retry" line (see retryEnded): the goroutine logs it as its last act.
func probeRetry(anyError bool) (installRetry, *lockedBuffer) {
	var logs lockedBuffer
	return installRetry{Goroutine: "probe", What: "probe", AnyError: anyError, Log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

// retryEnded waits until the retry goroutine has logged that the write landed, which is the last thing it does.
func retryEnded(logs *lockedBuffer) {
	GinkgoHelper()
	Eventually(logs.String).Should(ContainSubstring("recorded after a retry"))
}

// lockedBuffer is a bytes.Buffer that the retry goroutine and the spec can use together.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ = Describe("startup install audit writes before the writer role is granted", func() {
	denied := fmt.Errorf("audit: record install event x: %w: permission denied for table install_audit_events", audit.ErrInstallWriteDenied)

	It("retries a refused write until it is recorded, and stops with the server", func(ctx SpecContext) {
		calls := 0
		installWriteLoop(ctx, slog.Default(), "probe", "", time.Millisecond, func(context.Context) error {
			calls++
			if calls < 3 {
				return denied
			}
			return nil
		})
		Expect(calls).To(Equal(3))

		stopped, cancel := context.WithCancel(ctx)
		cancel()
		installWriteLoop(stopped, slog.Default(), "probe", "", time.Hour, func(context.Context) error {
			Fail("no write after the server stopped")
			return nil
		})
	})

	It("starts the one retry only for a refused install write (or any error when asked), and leaves other errors to the caller", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		landed := make(chan struct{})
		write := func(context.Context) error { close(landed); return nil }

		r, logs := probeRetry(false)
		Expect(retryInstallWrite(bg, cancel, r, time.Millisecond, nil, write)).To(Succeed(), "nothing to retry")

		other := errors.New("not a refusal")
		Expect(retryInstallWrite(bg, cancel, r, time.Millisecond, other, write)).To(MatchError(other))
		Consistently(landed, "50ms").ShouldNot(BeClosed(), "another error does not start a retry")

		Expect(retryInstallWrite(bg, cancel, r, time.Millisecond, denied, write)).To(Succeed())
		Eventually(landed).Should(BeClosed())
		retryEnded(logs)
	})

	It("logs a refused write once for the same state: the first denial, then the landing", func(ctx SpecContext) {
		r, logs := probeRetry(false)
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		calls := 0
		// The sequence is denied (the run that has just happened), denied (the first retry), then success.
		Expect(retryInstallWrite(bg, cancel, r, time.Millisecond, denied, func(context.Context) error {
			calls++
			if calls < 2 {
				return denied
			}
			return nil
		})).To(Succeed())
		retryEnded(logs)

		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		Expect(lines).To(HaveLen(2), "one line for the first denial, one for the landing: %v", lines)
		Expect(lines[0]).To(ContainSubstring("the probe is not recorded yet:"))
		Expect(lines[1]).To(ContainSubstring("recorded after a retry"))
		Expect(logs.String()).NotTo(ContainSubstring("still not recorded"))
	})

	It("retries any error when asked to, as the key-set check does", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		landed := make(chan struct{})
		r, logs := probeRetry(true)
		Expect(retryInstallWrite(bg, cancel, r, time.Millisecond,
			errors.New("a hiccup"), func(context.Context) error { close(landed); return nil })).To(Succeed())
		Eventually(landed).Should(BeClosed())
		retryEnded(logs)
	})

	It("does not let a hung first attempt hold the start, and still retries in the background", func(ctx SpecContext) {
		bg, cancel := context.WithCancel(ctx)
		defer cancel()
		calls := 0
		landed := make(chan struct{})
		returned := make(chan error, 1)
		r, logs := probeRetry(true)
		go func() {
			returned <- startInstallWrite(bg, cancel, r, time.Millisecond, 50*time.Millisecond,
				func(c context.Context) error {
					calls++
					if calls == 1 {
						<-c.Done() // a database that does not answer: only the first attempt's bound ends it
						return c.Err()
					}
					close(landed)
					return nil
				})
		}()
		Eventually(returned, "2s").Should(Receive(BeNil()), "control returns within about the first-attempt timeout")
		Eventually(landed).Should(BeClosed(), "the background retry still runs")
		retryEnded(logs)
	})

	It("waits in the grant-only migrate for the role, and gives up with the wait", func(ctx SpecContext) {
		calls := 0
		Expect(grantInstallAuditWriter(ctx, time.Minute, time.Millisecond, func(context.Context) (bool, error) {
			calls++
			return calls == 3, nil
		})).To(Succeed())
		Expect(calls).To(Equal(3))

		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		Expect(grantInstallAuditWriter(short, 20*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
			return false, nil
		})).To(MatchError(ContainSubstring("did not appear within 20ms")))

		Expect(grantInstallAuditWriter(ctx, time.Minute, time.Millisecond, func(context.Context) (bool, error) {
			return false, errors.New("permission denied for table install_audit_events")
		})).To(MatchError(ContainSubstring("grant the install audit writer")), "a grant that fails is not waited out")
	})

	// An external database: nothing creates the role after the migrate Job, so the chart passes
	// --require-install-writer and the Job fails with the remedy (the cnpg upgrade only warns). The
	// check runs before any migration, so a failed Job leaves the schema as it was.
	It("fails the migration without the role when it is required, and only warns otherwise", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		dsn, stop, err := pgtest.StartServer(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)
		conn, err := pgx.Connect(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = conn.Close(ctx) })
		untouched := func() {
			GinkgoHelper()
			var goose, tables bool
			Expect(conn.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL, to_regclass('install_audit_events') IS NOT NULL`).
				Scan(&goose, &tables)).To(Succeed())
			Expect(goose).To(BeFalse(), "no migration ran")
			Expect(tables).To(BeFalse(), "no migration ran")
		}
		exec := func(stmt string) {
			GinkgoHelper()
			_, err := conn.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), stmt)
		}
		required := migrateOptions{requireWriter: true}

		err = migrate(ctx, dsn, time.Minute, required)
		Expect(err).To(MatchError(ContainSubstring("the role koinon_install_audit_writer does not exist")))
		Expect(err).To(MatchError(ContainSubstring(project.SecretKeysDocURL)))
		Expect(err).To(MatchError(ContainSubstring("no migration was applied")))
		untouched()

		By("without --require-install-writer nothing is checked: the cnpg pre-upgrade run goes on")
		Expect(migrate(ctx, dsn, time.Minute, migrateOptions{})).To(Succeed())

		By("the operator created the role; membership is koinon-api's own check, not the migration's")
		exec(`CREATE ROLE koinon_install_audit_writer NOLOGIN`)
		Expect(migrate(ctx, dsn, time.Minute, required)).To(Succeed())
	}, SpecTimeout(3*time.Minute))
})

var _ = Describe("peekExportOrg", func() {
	It("returns the header's org and a reader that still yields the whole stream", func() {
		const stream = `{"format":"x","chain":"org","org_id":"00000000-0000-4000-8000-0000000000bb"}` + "\nrest\n"
		org, r := peekExportOrg(strings.NewReader(stream))
		Expect(org).To(Equal("00000000-0000-4000-8000-0000000000bb"))
		b, err := io.ReadAll(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal(stream))
	})
	It("returns no org for a line that is not a header, and loses nothing", func() {
		org, r := peekExportOrg(strings.NewReader("not json"))
		Expect(org).To(BeEmpty())
		b, _ := io.ReadAll(r)
		Expect(string(b)).To(Equal("not json"))
	})
})
