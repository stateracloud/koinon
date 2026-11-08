// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the Helm-set platform settings", func() {
	serveFlags := func() *pflag.FlagSet { return newServeCommand().Flags() }

	Describe("the flag table", func() {
		It("puts every flag of koinon-api serve on the recorded table or on the list of flags left out", func() {
			recorded := map[string]bool{}
			for _, s := range recordedSettings {
				recorded[s.Flag] = true
			}
			var undecided, both []string
			serveFlags().VisitAll(func(f *pflag.Flag) {
				_, left := unrecordedSettings[f.Name]
				switch {
				case recorded[f.Name] && left:
					both = append(both, f.Name)
				case !recorded[f.Name] && !left:
					undecided = append(undecided, f.Name)
				}
			})
			Expect(undecided).To(BeEmpty(), "decide for each new flag whether platform.settings.observed records it: add it to recordedSettings, or to unrecordedSettings with the reason (ADR 0100 decision 3)")
			Expect(both).To(BeEmpty(), "a flag is recorded or left out, not both")
		})

		It("names no flag that koinon-api serve does not have", func() {
			fs := serveFlags()
			for _, s := range recordedSettings {
				Expect(fs.Lookup(s.Flag)).NotTo(BeNil(), "recordedSettings names --%s", s.Flag)
			}
			for name, why := range unrecordedSettings {
				Expect(fs.Lookup(name)).NotTo(BeNil(), "unrecordedSettings names --%s", name)
				Expect(why).NotTo(BeEmpty())
			}
		})

		It("leaves the price refresh flags to price.refresh.settings_changed", func() {
			for _, name := range []string{"prices-refresh", "prices-refresh-interval", "prices-refresh-max-change-percent", "prices-refresh-max-auto-per-day"} {
				Expect(unrecordedSettings).To(HaveKey(name))
			}
		})

		It("uses a distinct summary key for every setting", func() {
			seen := map[string]bool{imageVersionKey: true}
			for _, s := range recordedSettings {
				Expect(seen).NotTo(HaveKey(s.Key))
				seen[s.Key] = true
			}
		})

		It("has summary keys that pass the audit scrubber", func(ctx SpecContext) {
			settings, err := platformSettings(serveFlags())
			Expect(err).NotTo(HaveOccurred())
			fake := &audit.Fake{}
			_, err = fake.InTx(ctx, func(_ *store.Queries, b audit.Batch) error {
				b.Install(audit.InstallEvent{Actor: audit.System("koinon-test:spec"), Action: actionSettingsObserved, TargetKind: "platform", TargetID: "settings", After: settings})
				return nil
			})
			Expect(err).NotTo(HaveOccurred(), "a flag whose name looks like a secret needs another Key")
			Expect(fake.InstallEvents()).To(HaveLen(1))
		})
	})

	Describe("platformSettings", func() {
		It("records every listed setting, and the image's version", func() {
			settings, err := platformSettings(serveFlags())
			Expect(err).NotTo(HaveOccurred())
			Expect(settings).To(HaveLen(len(recordedSettings) + 1))
			Expect(settings).To(HaveKeyWithValue("bootstrap-login", "true"))
			Expect(settings).To(HaveKeyWithValue("session-idle-timeout", "2h0m0s"))
			Expect(settings).To(HaveKeyWithValue(imageVersionKey, project.Version))
		})

		It("records the name of a Secret, a file or a directory and never opens it", func() {
			dir := GinkgoT().TempDir()
			pw := filepath.Join(dir, "bootstrap-password")
			Expect(os.WriteFile(pw, []byte("koinon_test_not-a-real-password"), 0o600)).To(Succeed())
			fs := serveFlags()
			for flag, v := range map[string]string{
				"bootstrap-password-file": pw,
				"kek-secret":              "koinon-kek",
				"kek-dir":                 filepath.Join(dir, "does-not-exist"),
				"saved-work-secret":       "koinon-saved-work",
				"otlp-token-dir":          filepath.Join(dir, "no-such-token-dir"),
			} {
				Expect(fs.Set(flag, v)).To(Succeed())
			}
			settings, err := platformSettings(fs)
			Expect(err).NotTo(HaveOccurred(), "a path that does not exist is fine: it is not opened")
			Expect(settings).To(HaveKeyWithValue("bootstrap-admin-pw-file", pw))
			Expect(settings).To(HaveKeyWithValue("kek-object", "koinon-kek"))
			Expect(settings).To(HaveKeyWithValue("saved-work-object", "koinon-saved-work"))
			raw, err := json.Marshal(settings)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring("koinon_test_not-a-real-password"), "no file content")
		})

		It("drops the user info and the query of a URL", func() {
			fs := serveFlags()
			Expect(fs.Set("gateway-url", "http://user:koinon_test_not-a-real-password@gateway.koinon.svc:8080/v1?key=koinon_test_not-a-real-token#x")).To(Succeed())
			Expect(fs.Set("github-api-url", "not a url")).To(Succeed())
			settings, err := platformSettings(fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings).To(HaveKeyWithValue("gateway-url", "http://gateway.koinon.svc:8080/v1"))
			Expect(settings).To(HaveKeyWithValue("github-api-url", "(set, not a URL)"))
		})

		It("follows the flags the process was started with", func() {
			fs := serveFlags()
			Expect(fs.Set("chart-version", "0.3.1")).To(Succeed())
			Expect(fs.Set("public-url", "https://koinon.example.test/")).To(Succeed())
			settings, err := platformSettings(fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings).To(HaveKeyWithValue("chart-version", "0.3.1"))
			Expect(settings).To(HaveKeyWithValue("public-url", "https://koinon.example.test/"))
		})
	})

	Describe("diffSettings", func() {
		It("returns every setting on a first start, with no before", func() {
			before, after := diffSettings(nil, map[string]string{"a": "1", "b": "2"})
			Expect(before).To(BeNil())
			Expect(after).To(Equal(map[string]string{"a": "1", "b": "2"}))
		})

		It("returns nothing when nothing changed", func() {
			before, after := diffSettings(map[string]string{"a": "1"}, map[string]string{"a": "1"})
			Expect(before).To(BeNil())
			Expect(after).To(BeNil())
		})

		It("returns only the changed key", func() {
			before, after := diffSettings(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "3"})
			Expect(before).To(Equal(map[string]string{"b": "2"}))
			Expect(after).To(Equal(map[string]string{"b": "3"}))
		})

		It("returns a setting that is new, and one that left the table", func() {
			before, after := diffSettings(map[string]string{"a": "1", "gone": "x"}, map[string]string{"a": "1", "new": "y"})
			Expect(before).To(Equal(map[string]string{"gone": "x"}))
			Expect(after).To(Equal(map[string]string{"new": "y"}))
		})

		It("treats a value that became empty as a change", func() {
			before, after := diffSettings(map[string]string{"a": "1"}, map[string]string{"a": ""})
			Expect(before).To(Equal(map[string]string{"a": "1"}))
			Expect(after).To(Equal(map[string]string{"a": ""}))
		})
	})

	Describe("foldSettings", func() {
		event := func(seq int64, before, after map[string]string) store.InstallAuditEvent {
			raw, err := json.Marshal(map[string]any{"before": before, "after": after})
			Expect(err).NotTo(HaveOccurred())
			return store.InstallAuditEvent{ChainSeq: seq, Summary: raw}
		}

		It("is nil without events", func() {
			state, err := foldSettings(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(BeNil())
		})

		It("applies the events in turn", func() {
			state, err := foldSettings([]store.InstallAuditEvent{
				event(1, nil, map[string]string{"a": "1", "b": "2", "c": "3"}),
				event(2, map[string]string{"b": "2"}, map[string]string{"b": "9"}),
				event(3, map[string]string{"c": "3"}, nil),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(state).To(Equal(map[string]string{"a": "1", "b": "9"}))
		})
	})

	Describe("on Postgres", func() {
		var (
			pool *pgxpool.Pool
			run  audit.Runner
		)
		settingsEvents := func(ctx context.Context) []map[string]any {
			GinkgoHelper()
			rows, err := pool.Query(ctx, `SELECT summary FROM install_audit_events WHERE action = 'platform.settings.observed' ORDER BY chain_seq`)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()
			var out []map[string]any
			for rows.Next() {
				var raw []byte
				Expect(rows.Scan(&raw)).To(Succeed())
				out = append(out, audit.SummaryOf(raw))
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			return out
		}
		base := func() map[string]string {
			return map[string]string{"public-url": "https://koinon.example.test", "durable": "false", imageVersionKey: "v0.0.0-test"}
		}

		BeforeEach(func(ctx SpecContext) {
			pool = pgtest.Start(ctx)
			run = audit.NewRunner(pool)
		})

		It("writes one event with every setting on the first start, and nothing on an unchanged restart", func(ctx SpecContext) {
			wrote, err := observeSettings(ctx, run, base())
			Expect(err).NotTo(HaveOccurred())
			Expect(wrote).To(BeTrue())
			wrote, err = observeSettings(ctx, run, base())
			Expect(err).NotTo(HaveOccurred())
			Expect(wrote).To(BeFalse())

			events := settingsEvents(ctx)
			Expect(events).To(HaveLen(1))
			Expect(events[0]).NotTo(HaveKey("before"))
			Expect(events[0]["after"]).To(Equal(map[string]any{"public-url": "https://koinon.example.test", "durable": "false", imageVersionKey: "v0.0.0-test"}))
			var actorKind, actorID string
			Expect(pool.QueryRow(ctx, `SELECT actor_kind, actor_id FROM install_audit_events WHERE action = 'platform.settings.observed'`).Scan(&actorKind, &actorID)).To(Succeed())
			Expect([]string{actorKind, actorID}).To(Equal([]string{"system", "koinon-api:startup"}))
		})

		It("writes one event with only the changed key when a value changes, and compares with all events so far", func(ctx SpecContext) {
			_, err := observeSettings(ctx, run, base())
			Expect(err).NotTo(HaveOccurred())
			changed := base()
			changed["durable"] = "true"
			wrote, err := observeSettings(ctx, run, changed)
			Expect(err).NotTo(HaveOccurred())
			Expect(wrote).To(BeTrue())
			wrote, err = observeSettings(ctx, run, changed)
			Expect(err).NotTo(HaveOccurred())
			Expect(wrote).To(BeFalse(), "the second event holds one key, the rest comes from the first")

			events := settingsEvents(ctx)
			Expect(events).To(HaveLen(2))
			Expect(events[1]["before"]).To(Equal(map[string]any{"durable": "false"}))
			Expect(events[1]["after"]).To(Equal(map[string]any{"durable": "true"}))
		})

		It("writes one event when two replicas start at once", func(ctx SpecContext) {
			const replicas = 4
			var wg sync.WaitGroup
			wrote := make([]bool, replicas)
			errs := make([]error, replicas)
			for i := range replicas {
				wg.Go(func() { wrote[i], errs[i] = observeSettings(ctx, run, base()) })
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(slices.DeleteFunc(wrote, func(w bool) bool { return !w })).To(HaveLen(1))
			Expect(settingsEvents(ctx)).To(HaveLen(1))
		})
	})
})
