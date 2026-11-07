// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// keyEvent is one install audit event of a key operation, as the chain holds it.
type keyEvent struct {
	ActorKind, ActorID, Action, TargetKind, TargetID string
	Summary                                          map[string]any
	Raw                                              string
}

// auditMark is the chain position now; keyEventsSince returns the key events written after it.
// The install audit log refuses DELETE and TRUNCATE, so specs read what they wrote instead of
// clearing the table.
func auditMark(ctx context.Context) int64 {
	GinkgoHelper()
	var seq int64
	Expect(pool.QueryRow(ctx, `SELECT coalesce(max(chain_seq), 0) FROM install_audit_events`).Scan(&seq)).To(Succeed())
	return seq
}

func keyEventsSince(ctx context.Context, mark int64) []keyEvent {
	GinkgoHelper()
	rows, err := pool.Query(ctx, `SELECT actor_kind, actor_id, action, target_kind, target_id, summary
		FROM install_audit_events WHERE chain_seq > $1 AND starts_with(action, 'keys.') AND action <> 'keys.forget' ORDER BY chain_seq`, mark)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var out []keyEvent
	for rows.Next() {
		var e keyEvent
		var raw []byte
		Expect(rows.Scan(&e.ActorKind, &e.ActorID, &e.Action, &e.TargetKind, &e.TargetID, &raw)).To(Succeed())
		e.Raw = string(raw)
		Expect(json.Unmarshal(raw, &e.Summary)).To(Succeed())
		out = append(out, e)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return out
}

func actions(es []keyEvent) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Action)
	}
	return out
}

// section returns the object under key ("before" or "after") of an event's summary.
func section(e keyEvent, key string) map[string]any {
	GinkgoHelper()
	m, ok := e.Summary[key].(map[string]any)
	Expect(ok).To(BeTrue(), "%s has no %q in its summary: %s", e.Action, key, e.Raw)
	return m
}

func kekIDs(state map[string]any) []string {
	var ids []string
	for _, k := range state["keks"].([]any) {
		ids = append(ids, k.(map[string]any)["id"].(string))
	}
	return ids
}

func strs(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// recordLive records the key set load returns as the chain's last state (a start-up observation with
// no generated Secrets), so the next command or check starts from it.
func recordLive(ctx context.Context, load keyscli.KeyLoader) {
	GinkgoHelper()
	_, err := (&keyscli.Auditor{Pool: pool, Authoritative: true}).Observe(ctx, load)
	Expect(err).NotTo(HaveOccurred())
}

// loaderOf returns a loader of set that hands out copies of its keys: a check clears the keys it loaded.
func loaderOf(set *keyscli.KeySet) keyscli.KeyLoader {
	return func(context.Context) (keyscli.KeySet, error) {
		c := *set
		c.Keys = map[string][]byte{}
		for id, k := range set.Keys {
			c.Keys[id] = append([]byte(nil), k...)
		}
		return c, nil
	}
}

var _ = Describe("key events in the install audit chain", func() {
	var (
		kube *fake.Clientset
		ks   keyscli.KubeStore
		tool *keyscli.Tool
		out  *bytes.Buffer
		now  time.Time
		org  pgtype.UUID
		user pgtype.UUID
	)
	const first = "kek-20260101-aaaa"

	newKeySet := func(ctx SpecContext) keyscli.KeySet {
		GinkgoHelper()
		set, err := ks.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		return set
	}
	// noKeyMaterial fails when any event carries a KEK, in the Secret's encoding or raw.
	noKeyMaterial := func(ctx SpecContext, es []keyEvent) {
		GinkgoHelper()
		set := newKeySet(ctx)
		for id, k := range set.Keys {
			for _, form := range []string{string(secretstore.EncodeKey(k)), base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k), string(k)} {
				for _, e := range es {
					Expect(e.Raw).NotTo(ContainSubstring(form), "the %s event carries the key %s", e.Action, id)
				}
			}
		}
	}

	BeforeEach(func(ctx SpecContext) {
		pgtest.TruncateOrgs(ctx, pool)
		_, err := pool.Exec(ctx, `DELETE FROM kek_fingerprints`)
		Expect(err).NotTo(HaveOccurred())
		now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		kube = fake.NewClientset()
		raw := make([]byte, secretstore.KeySize)
		copy(raw, "koinon_test_not-a-real-kek-aaaa")
		_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "koinon-kek", Namespace: ns},
			Data:       map[string][]byte{first: secretstore.EncodeKey(raw), "active": []byte(first)},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ks = keyscli.KubeStore{Client: kube, Namespace: ns, Name: "koinon-kek"}
		out = &bytes.Buffer{}
		tool = &keyscli.Tool{Pool: pool, Store: ks, Out: out, Now: func() time.Time { return now }}
		org, user, _ = pgtest.NewOrg(ctx, pool)
		// The chain's last state is whatever an earlier spec left: record this Secret as it is, so a
		// command's "before" is not preceded by a keys.observed that only reflects the leftovers.
		recordLive(ctx, ks.Load)
	})

	Describe("the commands", func() {
		It("writes one event for add, activate and retire, with the key-set state before and after", func(ctx SpecContext) {
			mark := auditMark(ctx)
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(Succeed())
			now = now.Add(6 * time.Minute)
			Expect(tool.Retire(ctx, first)).To(Succeed())

			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionAdd, keyscli.ActionActivate, keyscli.ActionRetire}))
			for i, cmd := range []string{"add", "activate", "retire"} {
				Expect(es[i].ActorKind).To(Equal("cli"))
				Expect(es[i].ActorID).To(Equal("keys " + cmd))
				Expect(es[i].TargetKind).To(Equal("kek"))
			}

			add, activate, retire := es[0], es[1], es[2]
			Expect(add.TargetID).To(Equal("kek-20261023-bbbb"))
			Expect(kekIDs(section(add, "before"))).To(Equal([]string{first}))
			Expect(kekIDs(section(add, "after"))).To(Equal([]string{first, "kek-20261023-bbbb"}))
			Expect(section(add, "after")["active"]).To(Equal(first), "add does not activate")
			for _, k := range section(add, "after")["keks"].([]any) {
				Expect(k.(map[string]any)["fingerprint"]).NotTo(BeEmpty())
			}

			Expect(section(activate, "before")["active"]).To(Equal(first))
			Expect(section(activate, "after")["active"]).To(Equal("kek-20261023-bbbb"))

			Expect(kekIDs(section(retire, "before"))).To(HaveLen(2))
			Expect(kekIDs(section(retire, "after"))).To(Equal([]string{"kek-20261023-bbbb"}))
			Expect(strs(section(retire, "after")["retired"])).To(ContainElement(first))
			noKeyMaterial(ctx, es)
		})

		It("does not write an event for a command that refused", func(ctx SpecContext) {
			mark := auditMark(ctx)
			Expect(tool.Add(ctx, first)).To(MatchError(ContainSubstring("already exists")))
			Expect(tool.Activate(ctx, "kek-20261023-zzzz")).To(MatchError(ContainSubstring("not in the key set")))
			Expect(tool.Retire(ctx, first)).To(MatchError(ContainSubstring("active KEK")))
			Expect(keyEventsSince(ctx, mark)).To(BeEmpty())
		})

		It("keeps the change and says so when the event cannot be written", func(ctx SpecContext) {
			tool.Audit = &keyscli.Auditor{Pool: pool, Write: func(context.Context, audit.InstallEvent) error { return errors.New("koinon_test database down") }}
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(MatchError(keyscli.ErrNotAudited))
			Expect(newKeySet(ctx).Keys).To(HaveKey("kek-20261023-bbbb"), "the change stands")
		})
	})

	Describe("rewrap and reencrypt", func() {
		var ids []string
		BeforeEach(func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			// Providers sealed under the first KEK, then a new active KEK.
			svc := catalog.New(pool, func() *secretstore.Keyring {
				set := newKeySet(ctx)
				r, err := secretstore.NewKeyring(set.Keys, first)
				Expect(err).NotTo(HaveOccurred())
				return r
			}(), nil)
			ids = nil
			for i := range 4 {
				p, err := svc.CreateProvider(ctx, org, audit.User(user), user, catalog.ProviderInput{
					Name: pgtest.Uniq("p"), Type: "openai", URL: "https://x.example.com/v1", APIKey: "sk-koinon_test_not-a-real-" + string(rune('a'+i)),
				})
				Expect(err).NotTo(HaveOccurred())
				ids = append(ids, p.ID)
			}
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(Succeed())
		})

		It("writes one event per run, after the loop, with the counts, the failures by kind and the usage", func(ctx SpecContext) {
			_, err := pool.Exec(ctx, `UPDATE providers SET api_key = jsonb_set(api_key, '{kek_id}', '"gone"') WHERE id = $1`, ids[0])
			Expect(err).NotTo(HaveOccurred())

			mark := auditMark(ctx)
			err = tool.Rewrap(ctx)
			Expect(err).To(HaveOccurred(), "the failed row is reported")
			Expect(errors.Is(err, keyscli.ErrNotAudited)).To(BeFalse())

			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionRewrap}))
			e := es[0]
			Expect(e.ActorKind).To(Equal("cli"))
			Expect(e.ActorID).To(Equal("keys rewrap"))
			after := section(e, "after")
			Expect(after["done"]).To(BeEquivalentTo(3))
			Expect(after["failed"]).To(BeEquivalentTo(1))
			Expect(after["failures"]).To(Equal(map[string]any{"unknown_kek": float64(1)}))
			Expect(after["active"]).To(Equal("kek-20261023-bbbb"))
			Expect(after["kek_usage"]).To(Equal(map[string]any{"kek-20261023-bbbb": float64(3), "gone": float64(1)}))
			Expect(section(e, "before")["kek_usage"]).To(Equal(map[string]any{first: float64(3), "gone": float64(1)}))
			noKeyMaterial(ctx, es)
		})

		It("writes nothing for a run that changes nothing", func(ctx SpecContext) {
			Expect(tool.Rewrap(ctx)).To(Succeed())
			mark := auditMark(ctx)
			tool.RecordStarted = true
			Expect(tool.Rewrap(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("0 value(s) rewrapped"))
			Expect(keyEventsSince(ctx, mark)).To(BeEmpty())
		})

		It("writes .started before the loop when asked, so a run that dies shows", func(ctx SpecContext) {
			mark := auditMark(ctx)
			tool.RecordStarted = true
			Expect(tool.Rewrap(ctx)).To(Succeed())
			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionRewrap + keyscli.StartedSuffix, keyscli.ActionRewrap}))
			Expect(section(es[0], "after")["rows"]).To(BeEquivalentTo(4))
			Expect(section(es[1], "after")["done"]).To(BeEquivalentTo(4))
		})

		DescribeTable("a run in which every row fails still writes its closing event",
			func(ctx SpecContext, recordStarted bool, want []string) {
				_, err := pool.Exec(ctx, `UPDATE providers SET api_key = jsonb_set(api_key, '{kek_id}', '"gone"') WHERE id = ANY($1)`, ids)
				Expect(err).NotTo(HaveOccurred())
				mark := auditMark(ctx)
				tool.RecordStarted = recordStarted
				Expect(tool.Rewrap(ctx)).To(HaveOccurred())
				es := keyEventsSince(ctx, mark)
				Expect(actions(es)).To(Equal(want))
				closing := es[len(es)-1]
				Expect(section(closing, "after")["done"]).To(BeEquivalentTo(0))
				Expect(section(closing, "after")["failed"]).To(BeEquivalentTo(4))
				noKeyMaterial(ctx, es)
			},
			Entry("without --record-started", false, []string{keyscli.ActionRewrap}),
			Entry("with --record-started", true, []string{keyscli.ActionRewrap + keyscli.StartedSuffix, keyscli.ActionRewrap}),
		)

		It("writes the closing event of a run whose rows were all done between the count and the loop", func(ctx SpecContext) {
			other := &keyscli.Tool{Pool: pool, Store: ks, Out: io.Discard, Audit: &keyscli.Auditor{Pool: pool, Write: func(context.Context, audit.InstallEvent) error { return nil }}}
			tool.RecordStarted = true
			tool.Audit = &keyscli.Auditor{Pool: pool, Write: func(ctx context.Context, e audit.InstallEvent) error {
				if e.Action == keyscli.ActionRewrap+keyscli.StartedSuffix {
					// Out of band, after the count and before the loop: the pending rows are rewrapped.
					Expect(other.Rewrap(ctx)).To(Succeed())
				}
				return audit.RecordInstallErr(ctx, store.New(pool), e)
			}}
			mark := auditMark(ctx)
			Expect(tool.Rewrap(ctx)).To(Succeed())
			Expect(out.String()).To(ContainSubstring("0 value(s) rewrapped"))
			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionRewrap + keyscli.StartedSuffix, keyscli.ActionRewrap}), "the .started never stays open")
			Expect(section(es[1], "after")["done"]).To(BeEquivalentTo(0))
			Expect(section(es[1], "after")["failed"]).To(BeEquivalentTo(0))
		})

		It("records reencrypt the same way", func(ctx SpecContext) {
			mark := auditMark(ctx)
			Expect(tool.Reencrypt(ctx)).To(Succeed())
			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionReencrypt}))
			Expect(es[0].ActorID).To(Equal("keys reencrypt"))
			Expect(section(es[0], "after")["done"]).To(BeEquivalentTo(4))
			Expect(section(es[0], "after")["failed"]).To(BeEquivalentTo(0))
			noKeyMaterial(ctx, es)
		})
	})

	Describe("the start-up check (Observe)", func() {
		var (
			generated []keyscli.GeneratedSecret
			observer  *keyscli.Auditor
			set       keyscli.KeySet
		)
		kek := func(seed string) []byte {
			k := make([]byte, secretstore.KeySize)
			copy(k, "koinon_test_not-a-real-kek-"+seed)
			return k
		}
		observe := func(ctx SpecContext) (keyscli.ObserveResult, []keyEvent) {
			GinkgoHelper()
			mark := auditMark(ctx)
			res, err := observer.Observe(ctx, loaderOf(&set))
			Expect(err).NotTo(HaveOccurred())
			es := keyEventsSince(ctx, mark)
			if res.Wrote {
				Expect(es).To(HaveLen(1))
				Expect(es[0].Action).To(Equal(keyscli.ActionObserved))
				Expect(es[0].ActorKind).To(Equal("system"))
				Expect(es[0].ActorID).To(Equal("koinon-api:startup"))
			} else {
				Expect(es).To(BeEmpty())
			}
			return res, es
		}

		BeforeEach(func(ctx SpecContext) {
			generated = []keyscli.GeneratedSecret{{SecretName: "koinon-session", UID: "uid-1", CreatedAt: "2026-10-01T00:00:00Z"}}
			observer = &keyscli.Auditor{Pool: pool, Authoritative: true, Generated: func(context.Context) ([]keyscli.GeneratedSecret, error) { return generated, nil }}
			set = keyscli.KeySet{Keys: map[string][]byte{first: kek("aaaa")}, Active: first}
			observe(ctx) // the baseline: whatever earlier specs recorded, this is the last state now
		})

		It("writes nothing when the state is unchanged", func(ctx SpecContext) {
			res, _ := observe(ctx)
			Expect(res.Wrote).To(BeFalse())
			res, _ = observe(ctx)
			Expect(res.Wrote).To(BeFalse())
		})

		It("writes keys.observed for a new KEK, with the state before and after", func(ctx SpecContext) {
			set.Keys["kek-20261023-bbbb"] = kek("bbbb")
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			Expect(kekIDs(section(es[0], "before"))).To(Equal([]string{first}))
			Expect(kekIDs(section(es[0], "after"))).To(Equal([]string{first, "kek-20261023-bbbb"}))
			noKeyMaterial2(es, set)
			res, _ = observe(ctx)
			Expect(res.Wrote).To(BeFalse(), "recorded once")
		})

		It("writes keys.observed for a changed active KEK", func(ctx SpecContext) {
			set.Keys["kek-20261023-bbbb"] = kek("bbbb")
			observe(ctx)
			set.Active = "kek-20261023-bbbb"
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			Expect(section(es[0], "after")["active"]).To(Equal("kek-20261023-bbbb"))
		})

		It("writes keys.observed for a retired KEK, and lists it as retired", func(ctx SpecContext) {
			set.Keys["kek-20261023-bbbb"] = kek("bbbb")
			set.Active = "kek-20261023-bbbb"
			observe(ctx)
			delete(set.Keys, first)
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			Expect(kekIDs(section(es[0], "after"))).To(Equal([]string{"kek-20261023-bbbb"}))
			Expect(strs(section(es[0], "after")["retired"])).To(ContainElement(first))
		})

		It("writes keys.observed for a changed key under the same ID (a different fingerprint)", func(ctx SpecContext) {
			set.Keys[first] = kek("cccc")
			res, _ := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
		})

		It("writes keys.observed for a generated Secret that was re-created (new UID) or is new", func(ctx SpecContext) {
			generated[0].UID = "uid-2"
			generated[0].CreatedAt = "2026-11-01T00:00:00Z"
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			Expect(section(es[0], "after")["generated"]).To(ConsistOf(map[string]any{"secret_name": "koinon-session", "uid": "uid-2", "created_at": "2026-11-01T00:00:00Z"}))

			generated = append(generated, keyscli.GeneratedSecret{SecretName: "koinon-saved-work", UID: "uid-3", CreatedAt: "2026-11-01T00:00:00Z"})
			res, _ = observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			res, _ = observe(ctx)
			Expect(res.Wrote).To(BeFalse())
		})

		It("sees a KEK that no command made (an existing Secret, or the ensure-secrets Job) as keys.observed", func(ctx SpecContext) {
			// The baseline above recorded the first KEK: this one was never written by a command, only found.
			set = keyscli.KeySet{Keys: map[string][]byte{"kek-20260601-eeee": kek("eeee")}, Active: "kek-20260601-eeee"}
			generated = append(generated, keyscli.GeneratedSecret{SecretName: "koinon-kek", UID: "uid-kek", CreatedAt: "2026-06-01T00:00:00Z"})
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			Expect(kekIDs(section(es[0], "after"))).To(Equal([]string{"kek-20260601-eeee"}))
			noKeyMaterial2(es, set)
		})

		It("catches an activate and a retire whose event was not written", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			tool.Audit = &keyscli.Auditor{Pool: pool, Write: func(context.Context, audit.InstallEvent) error { return errors.New("koinon_test database down") }}
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(MatchError(keyscli.ErrNotAudited))
			now = now.Add(6 * time.Minute)
			Expect(tool.Retire(ctx, first)).To(MatchError(keyscli.ErrNotAudited))

			set = newKeySet(ctx)
			generated = nil
			res, es := observe(ctx)
			Expect(res.Wrote).To(BeTrue())
			after := section(es[0], "after")
			Expect(after["active"]).To(Equal("kek-20261023-bbbb"))
			Expect(kekIDs(after)).To(Equal([]string{"kek-20261023-bbbb"}))
			Expect(strs(after["retired"])).To(ContainElement(first))
			Expect(kekIDs(section(es[0], "before"))).To(ContainElement(first), "the last recorded state still had the retired KEK")
			res, _ = observe(ctx)
			Expect(res.Wrote).To(BeFalse())
		})

		It("keeps an activation whose event was lost: the next command records it as keys.observed before its own event", func(ctx SpecContext) {
			Expect(tool.Add(ctx, "kek-20261023-bbbb")).To(Succeed())
			tool.Audit = &keyscli.Auditor{Pool: pool, Write: func(context.Context, audit.InstallEvent) error { return errors.New("koinon_test database down") }}
			Expect(tool.Activate(ctx, "kek-20261023-bbbb")).To(MatchError(keyscli.ErrNotAudited))
			tool.Audit = nil

			mark := auditMark(ctx)
			Expect(tool.Add(ctx, "kek-20261023-cccc")).To(Succeed())
			es := keyEventsSince(ctx, mark)
			Expect(actions(es)).To(Equal([]string{keyscli.ActionObserved, keyscli.ActionAdd}))
			observed, add := es[0], es[1]
			Expect(observed.ActorKind).To(Equal("cli"), "the command that found the gap writes it")
			Expect(observed.ActorID).To(Equal("keys add"))
			Expect(section(observed, "before")["active"]).To(Equal(first))
			Expect(section(observed, "after")["active"]).To(Equal("kek-20261023-bbbb"))
			Expect(section(add, "before")["active"]).To(Equal("kek-20261023-bbbb"), "the add starts from the observed state")
			Expect(section(add, "after")["active"]).To(Equal("kek-20261023-bbbb"))

			By("the start-up check then has nothing left to record")
			set = newKeySet(ctx)
			res, _ := observe(ctx)
			Expect(res.Wrote).To(BeFalse())
		})

		It("loads the key set under the lock, so a change recorded while it waited is not reported", func(ctx SpecContext) {
			lock, err := pool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx SpecContext) { _ = lock.Rollback(ctx) })
			_, err = lock.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('koinon.keys_observed'))`)
			Expect(err).NotTo(HaveOccurred())

			loaded := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := observer.Observe(ctx, func(ctx context.Context) (keyscli.KeySet, error) {
					close(loaded)
					return ks.Load(ctx)
				})
				done <- err
			}()
			Consistently(loaded, 500*time.Millisecond).ShouldNot(BeClosed(), "nothing is loaded before the lock is held")

			By("a key is added and recorded while the check waits")
			before := newKeySet(ctx)
			added := newKeySet(ctx)
			added.Keys["kek-20261023-bbbb"] = kek("bbbb")
			Expect(ks.Save(ctx, added)).To(Succeed())
			recorder := &keyscli.Auditor{Pool: pool, Authoritative: true, Generated: observer.Generated}
			prevState, err := recorder.Snapshot(ctx, before)
			Expect(err).NotTo(HaveOccurred())
			afterState, err := recorder.After(ctx, newKeySet(ctx), prevState)
			Expect(err).NotTo(HaveOccurred())
			Expect(recorder.Record(ctx, keyscli.Event{Command: "add", Action: keyscli.ActionAdd, TargetKind: "kek", TargetID: "kek-20261023-bbbb", Before: prevState, After: afterState})).To(Succeed())

			mark := auditMark(ctx)
			Expect(lock.Rollback(ctx)).To(Succeed())
			Eventually(done).Should(Receive(BeNil()))
			Expect(keyEventsSince(ctx, mark)).To(BeEmpty(), "the check saw the recorded change, not a stale key set")
		})

		It("writes once when replicas start together", func(ctx SpecContext) {
			set.Keys["kek-20261023-bbbb"] = kek("bbbb")
			mark := auditMark(ctx)
			done := make(chan error, 3)
			for range 3 {
				go func() {
					_, err := observer.Observe(ctx, loaderOf(&set))
					done <- err
				}()
			}
			for range 3 {
				Expect(<-done).To(Succeed())
			}
			Expect(keyEventsSince(ctx, mark)).To(HaveLen(1))
		})
	})
})

// noKeyMaterial2 fails when an event carries a key of set.
func noKeyMaterial2(es []keyEvent, set keyscli.KeySet) {
	GinkgoHelper()
	for id, k := range set.Keys {
		for _, form := range []string{string(secretstore.EncodeKey(k)), base64.StdEncoding.EncodeToString(k), string(k)} {
			for _, e := range es {
				Expect(e.Raw).NotTo(ContainSubstring(form), "the %s event carries the key %s", e.Action, id)
			}
		}
	}
}

var _ = Describe("key-admin Job events", func() {
	const (
		jobNS  = "koinon-test"
		secret = "koinon-kek"
	)
	var (
		kube   *fake.Clientset
		kms    *secretstoretest.FakeKMS
		admin  secretstore.Wrappers
		out    *bytes.Buffer
		marker *memMarker
	)
	job := func(create bool, add string) keyscli.KeyAdmin {
		return keyscli.KeyAdmin{
			Store:  keyscli.KubeStore{Client: kube, Namespace: jobNS, Name: secret, Wrappers: admin},
			Create: create, AddID: add, Out: out, Marker: marker,
			Now:       func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) },
			HasValues: func(context.Context) (bool, error) { return false, nil },
			UsedID:    func(context.Context, string) (bool, error) { return false, nil },
			Audit: &keyscli.Auditor{Pool: pool, Actor: keyscli.ActorKeyAdmin, Generated: func(context.Context) ([]keyscli.GeneratedSecret, error) {
				return nil, nil
			}},
		}
	}

	jobStore := func() keyscli.KubeStore {
		return keyscli.KubeStore{Client: kube, Namespace: jobNS, Name: secret, Wrappers: admin}
	}

	BeforeEach(func(ctx SpecContext) {
		kube = fake.NewClientset()
		kms = &secretstoretest.FakeKMS{}
		marker = &memMarker{}
		var err error
		admin, err = secretstore.NewKeyAdminWrappers(marker.source(), kms)
		Expect(err).NotTo(HaveOccurred())
		admin = admin.WithInstallID(secretstore.StaticInstallID(secretstoretest.InstallA))
		out = &bytes.Buffer{}
		// The chain's last state is whatever an earlier spec left; these specs start from no Secret.
		recordLive(ctx, func(context.Context) (keyscli.KeySet, error) { return keyscli.KeySet{}, nil })
	})

	It("records the Secret it creates and the KEK it adds under the key-admin actor", func(ctx SpecContext) {
		mark := auditMark(ctx)
		Expect(job(true, "").Run(ctx)).To(Succeed())
		es := keyEventsSince(ctx, mark)
		Expect(actions(es)).To(Equal([]string{keyscli.ActionSecretCreated}))
		Expect(es[0].ActorKind).To(Equal("system"))
		Expect(es[0].ActorID).To(Equal("koinon-api:key-admin"))
		Expect(es[0].TargetKind).To(Equal("secret"))
		Expect(es[0].TargetID).To(Equal(secret))
		Expect(kekIDs(section(es[0], "after"))).To(HaveLen(1))

		mark = auditMark(ctx)
		Expect(job(true, "kek-20261103-bbbb").Run(ctx)).To(Succeed())
		es = keyEventsSince(ctx, mark)
		Expect(actions(es)).To(Equal([]string{keyscli.ActionAdd}))
		Expect(es[0].ActorID).To(Equal("koinon-api:key-admin"))
		Expect(es[0].TargetID).To(Equal("kek-20261103-bbbb"))
		Expect(kekIDs(section(es[0], "after"))).To(HaveLen(2))

		By("a rerun changes nothing and writes nothing")
		mark = auditMark(ctx)
		Expect(job(true, "kek-20261103-bbbb").Run(ctx)).To(Succeed())
		Expect(keyEventsSince(ctx, mark)).To(BeEmpty())

		By("the start-up check finds the key set as the Job recorded it, and writes nothing")
		res, err := (&keyscli.Auditor{Pool: pool, Authoritative: true}).Observe(ctx, jobStore().Load)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Wrote).To(BeFalse())
		Expect(keyEventsSince(ctx, mark)).To(BeEmpty())
	})

	It("records the binding of the KEKs as keys.bound, and the start-up check then writes nothing", func(ctx SpecContext) {
		prim := bytes.Repeat([]byte("p"), secretstore.KeySize) // obviously fake
		wrapped, err := kms.Wrap(ctx, prim, secretstoretest.LegacyKEKAAD("kek-20261001-prim"))
		Expect(err).NotTo(HaveOccurred())
		legacy, err := secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: wrapped}.Encode()
		Expect(err).NotTo(HaveOccurred())
		_, err = kube.CoreV1().Secrets(jobNS).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: jobNS},
			Data:       map[string][]byte{secretstore.ActiveFile: []byte("kek-20261001-prim"), "kek-20261001-prim": legacy},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		recordLive(ctx, jobStore().Load)

		a := job(false, "")
		a.RewrapUnbound = true
		mark := auditMark(ctx)
		Expect(a.Run(ctx)).To(Succeed())
		es := keyEventsSince(ctx, mark)
		Expect(actions(es)).To(Equal([]string{keyscli.ActionBound}))
		Expect(es[0].ActorID).To(Equal("koinon-api:key-admin"))
		Expect(es[0].TargetKind).To(Equal("keys"))
		Expect(es[0].TargetID).To(Equal("keyset"))
		bindingOf := func(e keyEvent, key string) any {
			return section(e, key)["keks"].([]any)[0].(map[string]any)["binding"]
		}
		Expect(bindingOf(es[0], "before")).To(Equal("unbound"))
		Expect(bindingOf(es[0], "after")).To(Equal("bound"))
		Expect(es[0].Raw).NotTo(ContainSubstring(base64.StdEncoding.EncodeToString(prim)))

		res, err := (&keyscli.Auditor{Pool: pool, Authoritative: true}).Observe(ctx, jobStore().Load)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Wrote).To(BeFalse())
	})

	It("records a retire whose event was lost as keys.observed, before the Job's own event", func(ctx SpecContext) {
		Expect(job(true, "kek-20261103-bbbb").Run(ctx)).To(Succeed())
		tool := &keyscli.Tool{
			Pool: pool, Store: jobStore(), Out: io.Discard, Wrappers: admin,
			Audit: &keyscli.Auditor{Pool: pool, Write: func(context.Context, audit.InstallEvent) error { return errors.New("koinon_test database down") }},
		}
		Expect(tool.Retire(ctx, "kek-20261103-bbbb")).To(MatchError(keyscli.ErrNotAudited))

		mark := auditMark(ctx)
		Expect(job(true, "kek-20261103-cccc").Run(ctx)).To(Succeed())
		es := keyEventsSince(ctx, mark)
		Expect(actions(es)).To(Equal([]string{keyscli.ActionObserved, keyscli.ActionAdd}))
		Expect(es[0].ActorID).To(Equal("koinon-api:key-admin"))
		Expect(strs(section(es[0], "after")["retired"])).To(ContainElement("kek-20261103-bbbb"))
		Expect(strs(section(es[1], "before")["retired"])).To(ContainElement("kek-20261103-bbbb"), "the add starts from the observed state")
		Expect(kekIDs(section(es[1], "after"))).To(HaveLen(2))
	})

	It("warns and carries on when it cannot read the chain, and waits for it once for the whole run", func(ctx SpecContext) {
		unmigrated, err := pgxpool.New(ctx, pgtest.Run(ctx)) // no install_audit_events table
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(unmigrated.Close)
		a := job(true, "kek-20261103-bbbb")
		a.Audit = &keyscli.Auditor{Pool: unmigrated, Actor: keyscli.ActorKeyAdmin, Wait: time.Second}

		start := time.Now()
		Expect(a.Run(ctx)).To(Succeed())
		// One retry pause is 2s; a budget per call would pay it for the Secret and again for the added KEK.
		Expect(time.Since(start)).To(BeNumerically("<", 3500*time.Millisecond))
		Expect(out.String()).To(And(ContainSubstring("WARNING"), ContainSubstring("could not be read"), ContainSubstring("keys.observed")))
		ks, err := jobStore().Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ks.Keys).To(HaveLen(2), "the Secret and the added KEK stand")
	})

	It("records the recovery of a lost active KEK as keys.add then keys.activate", func(ctx SpecContext) {
		lost, survivor := "kek-20260101-lost", "kek-20260102-surv"
		local := bytes.Repeat([]byte("l"), secretstore.KeySize) // obviously fake
		_, err := kube.CoreV1().Secrets(jobNS).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: jobNS},
			Data: map[string][]byte{
				secretstore.ActiveFile: []byte(lost),
				survivor:               []byte(base64.StdEncoding.EncodeToString(local)),
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		a := job(true, "kek-20261103-bbbb")
		a.ActivateIfNoActive = true
		recordLive(ctx, jobStore().LoadWithoutActive) // the Secret was provisioned before the Job; the chain has seen it
		mark := auditMark(ctx)
		Expect(a.Run(ctx)).To(Succeed())
		es := keyEventsSince(ctx, mark)
		Expect(actions(es)).To(Equal([]string{keyscli.ActionAdd, keyscli.ActionActivate}))
		for _, e := range es {
			Expect(e.ActorID).To(Equal("koinon-api:key-admin"))
			Expect(e.TargetID).To(Equal("kek-20261103-bbbb"))
		}
		Expect(section(es[1], "after")["active"]).To(Equal("kek-20261103-bbbb"))
		for _, e := range es {
			Expect(strings.Contains(e.Raw, base64.StdEncoding.EncodeToString(local))).To(BeFalse())
		}
	})

	It("warns, and still succeeds, when the chain cannot take the event", func(ctx SpecContext) {
		a := job(true, "")
		a.Audit.Write = func(context.Context, audit.InstallEvent) error { return audit.ErrInstallWriteDenied }
		Expect(a.Run(ctx)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("keys.observed"))
		list, err := kube.CoreV1().Secrets(jobNS).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.Items).To(HaveLen(1), "the Secret stands")
	})
})
