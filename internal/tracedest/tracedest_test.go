// SPDX-License-Identifier: Apache-2.0

package tracedest_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/export"
)

var (
	own       = tracedest.Authority{Owner: true, Member: true}
	adminOnly = tracedest.Authority{Member: true}
	outsider  = tracedest.Authority{} // a platform admin who is not a member of the org
)

func rate(f float64) *float64 { return &f }

const (
	fakeKey    = "koinon_test_not-a-real-token"
	fakeKeyTwo = "koinon_test_not-a-real-token-two"
)

// fakeBackend is a fake OTLP backend that answers as told.
type fakeBackend struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	reject int64
	redir  string
	hits   int
	orgs   []string
	hdr    http.Header
	onHit  func()
}

func newFakeBackend() *fakeBackend {
	b := &fakeBackend{status: 200}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		b.mu.Lock()
		hook := b.onHit
		b.mu.Unlock()
		if hook != nil {
			hook()
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.hits++
		b.hdr = r.Header.Clone()
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					for _, kv := range sp.GetAttributes() {
						if kv.GetKey() == "koinon.org" {
							b.orgs = append(b.orgs, kv.GetValue().GetStringValue())
						}
					}
				}
			}
		}
		if b.redir != "" {
			http.Redirect(w, r, b.redir, http.StatusFound)
			return
		}
		if b.status != 200 {
			w.WriteHeader(b.status)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: b.reject}})
		_, _ = w.Write(out)
	}))
	return b
}

func (b *fakeBackend) set(f func(b *fakeBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

func (b *fakeBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}

func uuidOf(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}

var _ = Describe("Service", func() {
	var (
		svc     *tracedest.Service
		be      *fakeBackend
		org     pgtype.UUID
		user    pgtype.UUID
		actor   audit.Actor
		auditor pgtype.UUID
	)
	BeforeEach(func(ctx SpecContext) {
		be = newFakeBackend()
		DeferCleanup(be.Close)
		host, _, err := net.SplitHostPort(be.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		svc, err = tracedest.New(pool, ring, catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{host}}, nil)
		Expect(err).NotTo(HaveOccurred())
		org, user, _ = pgtest.NewOrg(ctx, pool)
		auditor, _, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
		// The ceiling is platform-wide: every spec starts from the default.
		_, err = svc.SetPlatformCeiling(ctx, auditor, actor, tracedest.LevelFull)
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `DELETE FROM platform_trace_destination`)
		Expect(err).NotTo(HaveOccurred())
	})

	langfuse := func(name string) tracedest.Input {
		return tracedest.Input{
			Name: name, Backend: "langfuse", EndpointURL: be.URL + "/api/public/otel/v1/traces", Enabled: true,
			Secrets: map[string]string{"public_key": "koinon_test_not-a-real-public", "secret_key": fakeKey},
		}
	}
	events := func(ctx SpecContext, org pgtype.UUID, prefix string) []store.AuditEvent {
		GinkgoHelper()
		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 200})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if strings.HasPrefix(r.Action, prefix) {
				out = append(out, r)
			}
		}
		return out
	}

	Describe("test before save", func() {
		It("saves a destination the backend accepts, keeps the test result and seals the credentials", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("lf"))
			Expect(err).NotTo(HaveOccurred())
			Expect(d.LastTest).NotTo(BeNil())
			Expect(d.LastTest.OK).To(BeTrue())
			Expect(d.LastTest.TestedAt).To(BeTemporally("~", time.Now(), time.Minute))
			Expect(d.SecretFields).To(ConsistOf("public_key", "secret_key"))
			Expect(be.calls()).To(Equal(1))
			be.mu.Lock()
			Expect(be.hdr.Get("Authorization")).To(HavePrefix("Basic "))
			Expect(be.orgs).NotTo(BeEmpty())
			Expect(be.orgs).To(HaveEach(org.String()), "the test trace carries the org's ID")
			be.mu.Unlock()

			By("the row holds an envelope, not the key")
			var raw []byte
			Expect(pool.QueryRow(ctx, `SELECT credentials FROM trace_destinations WHERE org_id = $1 AND id = $2`, org, d.ID).Scan(&raw)).To(Succeed())
			Expect(string(raw)).To(ContainSubstring("ciphertext"))
			Expect(string(raw)).NotTo(ContainSubstring(fakeKey))

			By("the router can open it")
			routes, err := svc.Routes(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(1))
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKey))
			Expect(routes[0].Destination.Preset).To(Equal(export.Langfuse))
			Expect(routes[0].Level).To(Equal(tracedest.LevelNone))

			By("the audit record names the fields, not the values")
			ev := events(ctx, org, "trace.destination.create")
			Expect(ev).To(HaveLen(1))
			Expect(string(ev[0].Summary)).NotTo(ContainSubstring(fakeKey))
			Expect(string(ev[0].Summary)).To(ContainSubstring("secretFields"))
		})

		DescribeTable("saves nothing when the backend refuses the test trace",
			func(ctx SpecContext, arrange func(b *fakeBackend), reason string) {
				be.set(arrange)
				_, err := svc.Create(ctx, org, actor, user, own, langfuse("refused"))
				var failed *tracedest.TestFailedError
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(failed))
				Expect(err.Error()).To(ContainSubstring(reason))
				Expect(err.Error()).NotTo(ContainSubstring(fakeKey))
				list, err := svc.List(ctx, org)
				Expect(err).NotTo(HaveOccurred())
				Expect(list).To(BeEmpty())
				Expect(events(ctx, org, "trace.destination")).To(BeEmpty())
			},
			Entry("4xx", func(b *fakeBackend) { b.status = 401 }, "HTTP 401"),
			Entry("5xx", func(b *fakeBackend) { b.status = 503 }, "HTTP 503"),
			Entry("rejected spans in a 2xx", func(b *fakeBackend) { b.reject = 1 }, "rejected 1 span"),
			Entry("a redirect", func(b *fakeBackend) { b.redir = "https://elsewhere.example.com/" }, "redirect"),
		)

		It("refuses a host the egress policy denies, without sending anything", func(ctx SpecContext) {
			in := langfuse("denied")
			in.EndpointURL = "http://169.254.169.254/latest"
			_, err := svc.Create(ctx, org, actor, user, own, in)
			var inv *tracedest.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv))
			Expect(be.calls()).To(BeZero())
		})

		It("rejects bad input before it tests", func(ctx SpecContext) {
			for _, in := range []tracedest.Input{
				{Name: "Bad Name", Backend: "langfuse", EndpointURL: be.URL},
				{Name: "x", Backend: "honeycomb", EndpointURL: be.URL},
				{Name: "x", Backend: "datadog", EndpointURL: be.URL}, // no api_key
				{Name: "x", Backend: "datadog", EndpointURL: be.URL, Secrets: map[string]string{"api_key": fakeKey}, ResourceAttributes: map[string]string{"koinon.org": "other"}},
			} {
				_, err := svc.Create(ctx, org, actor, user, own, in)
				var inv *tracedest.InvalidError
				Expect(err).To(BeAssignableToTypeOf(inv), fmt.Sprint(in.Name, in.Backend))
			}
			Expect(be.calls()).To(BeZero())
		})

		It("refuses a second destination with the same name", func(ctx SpecContext) {
			_, err := svc.Create(ctx, org, actor, user, own, langfuse("dup"))
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.Create(ctx, org, actor, user, own, langfuse("dup"))
			Expect(err).To(MatchError(tracedest.ErrConflict))
		})
	})

	Describe("update", func() {
		It("merges secrets by name, tests the result and keeps the old version when the test fails", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("upd"))
			Expect(err).NotTo(HaveOccurred())

			By("a change without secrets keeps the stored ones")
			in := langfuse("upd")
			in.Secrets = nil
			in.ResourceAttributes = map[string]string{"deployment.environment": "prod"}
			d2, err := svc.Update(ctx, org, actor, own, d.ID, nil, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(d2.SecretFields).To(ConsistOf("public_key", "secret_key"))
			Expect(d2.ResourceAttributes).To(HaveKeyWithValue("deployment.environment", "prod"))
			routes, _ := svc.Routes(ctx, org)
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKey))

			By("one secret is replaced and the other stays")
			in.Secrets = map[string]string{"secret_key": fakeKeyTwo}
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			Expect(err).NotTo(HaveOccurred())
			routes, _ = svc.Routes(ctx, org)
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKeyTwo))
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("public_key", "koinon_test_not-a-real-public"))

			By("a failing test leaves the saved version alone")
			be.set(func(b *fakeBackend) { b.status = 403 })
			in.Secrets = map[string]string{"secret_key": "koinon_test_not-a-real-bad"}
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			var failed *tracedest.TestFailedError
			Expect(err).To(BeAssignableToTypeOf(failed))
			routes, _ = svc.Routes(ctx, org)
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKeyTwo))

			By("changing the backend starts from no secrets")
			be.set(func(b *fakeBackend) { b.status = 200 })
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, tracedest.Input{
				Name: "upd", Backend: "datadog", EndpointURL: be.URL, Enabled: true, Secrets: map[string]string{"api_key": fakeKey},
			})
			Expect(err).NotTo(HaveOccurred())
			routes, _ = svc.Routes(ctx, org)
			Expect(routes[0].Destination.Secrets).To(HaveLen(1))

			ev := events(ctx, org, "trace.destination.update")
			Expect(ev).To(HaveLen(3))
			for _, e := range ev {
				Expect(string(e.Summary)).NotTo(ContainSubstring(fakeKey))
			}
		})

		It("does not overwrite a change made meanwhile", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("race"))
			Expect(err).NotTo(HaveOccurred())
			// Another admin saves while this update's test trace is on its way.
			be.set(func(b *fakeBackend) {
				b.onHit = func() {
					_, err := pool.Exec(ctx, `UPDATE trace_destinations SET resource_attributes = '{"a":"b"}', config_version = config_version + 1 WHERE org_id = $1 AND id = $2`, org, d.ID)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, langfuse("race"))
			Expect(err).To(MatchError(tracedest.ErrChanged))
		})

		It("does not touch another org's destination", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("mine"))
			Expect(err).NotTo(HaveOccurred())
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			_, err = svc.Update(ctx, other, audit.User(otherUser), own, d.ID, nil, langfuse("mine"))
			Expect(err).To(MatchError(tracedest.ErrNotFound))
			Expect(svc.Delete(ctx, other, audit.User(otherUser), d.ID)).To(MatchError(tracedest.ErrNotFound))
			_, _, err = svc.Test(ctx, other, audit.User(otherUser), d.ID)
			Expect(err).To(MatchError(tracedest.ErrNotFound))
			list, _ := svc.List(ctx, other)
			Expect(list).To(BeEmpty())
		})
	})

	Describe("stored credentials and a changed endpoint", func() {
		It("sends stored secrets only to the endpoint origin they were stored for", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("keep"))
			Expect(err).NotTo(HaveOccurred())
			evil := newFakeBackend()
			DeferCleanup(evil.Close)
			evilHost, _, _ := net.SplitHostPort(evil.Listener.Addr().String())
			_ = evilHost // same host, different port: a different origin

			By("a new origin without the secrets is refused, and the evil backend receives nothing")
			in := langfuse("keep")
			in.EndpointURL = evil.URL + "/api/public/otel/v1/traces"
			in.Secrets = nil
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			var inv *tracedest.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv))
			Expect(evil.calls()).To(BeZero())

			By("only some secrets given: the others are not carried over")
			in.Secrets = map[string]string{"secret_key": fakeKeyTwo}
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			Expect(err).To(BeAssignableToTypeOf(inv))
			Expect(evil.calls()).To(BeZero())

			By("all secrets given: the new endpoint gets exactly those")
			in.Secrets = map[string]string{"public_key": "koinon_test_not-a-real-public-two", "secret_key": fakeKeyTwo}
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			Expect(err).NotTo(HaveOccurred())
			evil.mu.Lock()
			Expect(evil.hdr.Get("Authorization")).To(Equal("Basic " + base64.StdEncoding.EncodeToString([]byte("koinon_test_not-a-real-public-two:"+fakeKeyTwo))))
			evil.mu.Unlock()

			By("a path change on the same origin keeps the stored secrets")
			in2 := langfuse("keep")
			in2.EndpointURL = evil.URL + "/other/path"
			in2.Secrets = nil
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in2)
			Expect(err).NotTo(HaveOccurred())
			routes, _ := svc.Routes(ctx, org)
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKeyTwo))

			By("a changed backend needs its own secrets too")
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, tracedest.Input{Name: "keep", Backend: "datadog", EndpointURL: in2.EndpointURL, Enabled: true})
			Expect(err).To(BeAssignableToTypeOf(inv))
		})

		It("same for the platform destination", func(ctx SpecContext) {
			_, err := svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("platform"))
			Expect(err).NotTo(HaveOccurred())
			evil := newFakeBackend()
			DeferCleanup(evil.Close)
			in := langfuse("platform")
			in.EndpointURL = evil.URL
			in.Secrets = nil
			_, err = svc.SetPlatformDestination(ctx, auditor, actor, nil, in)
			var inv *tracedest.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv))
			Expect(evil.calls()).To(BeZero())
		})
	})

	Describe("changes that touch no connection field", func() {
		It("saves them untested, so a destination whose backend is down can be disabled", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("down"))
			Expect(err).NotTo(HaveOccurred())
			be.set(func(b *fakeBackend) { b.status = 503 })
			calls := be.calls()
			in := langfuse("down")
			in.Secrets = nil
			in.Enabled = false
			got, err := svc.Update(ctx, org, actor, own, d.ID, nil, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Enabled).To(BeFalse())
			Expect(be.calls()).To(Equal(calls), "no test trace was sent")
			Expect(got.LastTest).NotTo(BeNil())
			Expect(got.LastTest.OK).To(BeTrue(), "the earlier test result stays")
			Expect(got.SecretFields).To(ConsistOf("public_key", "secret_key"))
			Expect(got.ConfigVersion).To(Equal(d.ConfigVersion + 1))

			By("a changed endpoint is tested")
			in.EndpointURL += "x"
			_, err = svc.Update(ctx, org, actor, own, d.ID, nil, in)
			var failed *tracedest.TestFailedError
			Expect(err).To(BeAssignableToTypeOf(failed))
		})
	})

	Describe("config versions", func() {
		It("is not moved by the router's health writes, and a stale form is refused", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("ver"))
			Expect(err).NotTo(HaveOccurred())
			q := store.New(pool)
			id := uuidOf(d.ID)
			Expect(q.RecordTraceDestinationSuccess(ctx, store.RecordTraceDestinationSuccessParams{OrgID: org, ID: id})).To(Succeed())
			Expect(q.RecordTraceDestinationError(ctx, store.RecordTraceDestinationErrorParams{OrgID: org, ID: id, SpansDropped: 5})).To(Succeed())
			got, err := svc.Get(ctx, org, d.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ConfigVersion).To(Equal(d.ConfigVersion))
			Expect(got.Health.SpansDropped).To(Equal(int64(5)))

			in := langfuse("ver")
			in.Secrets = nil
			in.ResourceAttributes = map[string]string{"a": "b"}
			stale := d.ConfigVersion
			got, err = svc.Update(ctx, org, actor, own, d.ID, &stale, in)
			Expect(err).NotTo(HaveOccurred(), "health writes did not make the form stale")
			Expect(got.Health.SpansDropped).To(Equal(int64(5)), "an update keeps the router's health")

			in.ResourceAttributes = map[string]string{"a": "c"}
			_, err = svc.Update(ctx, org, actor, own, d.ID, &stale, in)
			Expect(err).To(MatchError(tracedest.ErrChanged))
		})

		It("does not write a re-test back over a destination changed or deleted meanwhile", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, langfuse("retest"))
			Expect(err).NotTo(HaveOccurred())
			be.set(func(b *fakeBackend) {
				b.onHit = func() {
					_, err := pool.Exec(ctx, `UPDATE trace_destinations SET enabled = false, config_version = config_version + 1 WHERE org_id = $1 AND id = $2`, org, d.ID)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			_, _, err = svc.Test(ctx, org, actor, d.ID)
			Expect(err).To(MatchError(tracedest.ErrChanged))
			got, _ := svc.Get(ctx, org, d.ID)
			Expect(got.Enabled).To(BeFalse(), "the concurrent change stays")

			be.set(func(b *fakeBackend) {
				b.onHit = func() {
					_, err := pool.Exec(ctx, `DELETE FROM trace_destinations WHERE org_id = $1 AND id = $2`, org, d.ID)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			_, _, err = svc.Test(ctx, org, actor, d.ID)
			Expect(err).To(MatchError(tracedest.ErrNotFound))
			_, err = svc.Get(ctx, org, d.ID)
			Expect(err).To(MatchError(tracedest.ErrNotFound), "a deleted destination is not brought back")
		})

		It("does not bring back a deleted platform destination", func(ctx SpecContext) {
			_, err := svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("platform"))
			Expect(err).NotTo(HaveOccurred())
			be.set(func(b *fakeBackend) {
				b.onHit = func() {
					_, err := pool.Exec(ctx, `DELETE FROM platform_trace_destination`)
					Expect(err).NotTo(HaveOccurred())
				}
			})
			_, _, err = svc.TestPlatform(ctx, auditor, actor)
			Expect(err).To(MatchError(tracedest.ErrNotFound))
			p, err := svc.GetPlatform(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Destination).To(BeNil())
		})
	})

	It("checks the platform destination's version", func(ctx SpecContext) {
		zero := int64(0)
		d, err := svc.SetPlatformDestination(ctx, auditor, actor, &zero, langfuse("platform"))
		Expect(err).NotTo(HaveOccurred(), "version 0 is 'no destination yet'")
		_, err = svc.SetPlatformDestination(ctx, auditor, actor, &zero, langfuse("platform"))
		Expect(err).To(MatchError(tracedest.ErrChanged))
		v := d.ConfigVersion
		in := langfuse("platform")
		in.Secrets = nil
		in.Enabled = false
		d2, err := svc.SetPlatformDestination(ctx, auditor, actor, &v, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(d2.ConfigVersion).To(Equal(v + 1))
		_, err = svc.SetPlatformDestination(ctx, auditor, actor, &v, in)
		Expect(err).To(MatchError(tracedest.ErrChanged))
	})

	Describe("endpoints", func() {
		It("normalises the URL before it is checked and stored", func(ctx SpecContext) {
			in := langfuse("upper")
			in.EndpointURL = strings.Replace(be.URL, "http://", "HTTP://", 1) + "/v1/traces"
			d, err := svc.Create(ctx, org, actor, user, own, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.EndpointURL).To(HavePrefix("http://"))
		})
		It("requires https unless the host is in the allowed hosts", func(ctx SpecContext) {
			plain, err := tracedest.New(pool, ring, catalog.URLPolicy{Namespace: "koinon"}, nil)
			Expect(err).NotTo(HaveOccurred())
			in := langfuse("plain")
			in.EndpointURL = "http://otlp.example.com/v1/traces"
			_, err = plain.Create(ctx, org, actor, user, own, in)
			var inv *tracedest.InvalidError
			Expect(err).To(BeAssignableToTypeOf(inv))
			Expect(err.Error()).To(ContainSubstring("https"))
		})
		It("lets Opik run without authentication", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, own, tracedest.Input{Name: "opik", Backend: "opik", EndpointURL: be.URL, Enabled: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.SecretFields).To(BeEmpty())
		})
	})

	Describe("who may add a destination while the level is full", func() {
		It("needs an owner to add or re-point one, not to disable or rename it", func(ctx SpecContext) {
			d, err := svc.Create(ctx, org, actor, user, adminOnly, langfuse("lvl"))
			Expect(err).NotTo(HaveOccurred(), "at level none an admin may add one")
			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())

			_, err = svc.Create(ctx, org, actor, user, adminOnly, langfuse("second"))
			Expect(err).To(MatchError(tracedest.ErrOwnerAtFull))
			in := langfuse("lvl")
			in.EndpointURL += "2"
			_, err = svc.Update(ctx, org, actor, adminOnly, d.ID, nil, in)
			Expect(err).To(MatchError(tracedest.ErrOwnerAtFull))

			quiet := langfuse("lvl")
			quiet.Secrets, quiet.Enabled = nil, false
			_, err = svc.Update(ctx, org, actor, adminOnly, d.ID, nil, quiet)
			Expect(err).NotTo(HaveOccurred(), "turning one off sends less")
			_, err = svc.Create(ctx, org, actor, user, own, langfuse("second"))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("a platform admin outside the org", func() {
		It("may lower the org's settings but not raise them", func(ctx SpecContext) {
			_, err := svc.UpdateSettings(ctx, org, actor, outsider, tracedest.SettingsInput{ContentLevel: "full", PlatformCopyLevel: "none"})
			Expect(err).To(MatchError(tracedest.ErrNotMember))
			_, err = svc.UpdateSettings(ctx, org, actor, outsider, tracedest.SettingsInput{ContentLevel: "redacted", PlatformCopyLevel: "none"})
			Expect(err).To(MatchError(tracedest.ErrNotMember))
			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", PlatformCopyLevel: "redacted"})
			Expect(err).NotTo(HaveOccurred())

			_, err = svc.UpdateSettings(ctx, org, actor, outsider, tracedest.SettingsInput{ContentLevel: "full", PlatformCopyLevel: "full"})
			Expect(err).To(MatchError(tracedest.ErrNotMember), "no raising the platform copy")
			st, err := svc.UpdateSettings(ctx, org, actor, outsider, tracedest.SettingsInput{ContentLevel: "redacted", PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())
			Expect(st.ContentLevel).To(Equal("redacted"))
			Expect(st.PlatformCopyLevel).To(Equal("none"))
		})
	})

	It("keeps the stored sample rate when an update leaves it out", func(ctx SpecContext) {
		_, err := svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "none", SampleRate: rate(0.4), PlatformCopyLevel: "none"})
		Expect(err).NotTo(HaveOccurred())
		st, err := svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "redacted", PlatformCopyLevel: "none"})
		Expect(err).NotTo(HaveOccurred())
		Expect(st.SampleRate).To(Equal(0.4))
		fresh, fu, _ := pgtest.NewOrg(ctx, pool)
		st, err = svc.UpdateSettings(ctx, fresh, audit.User(fu), own, tracedest.SettingsInput{ContentLevel: "none", PlatformCopyLevel: "none"})
		Expect(err).NotTo(HaveOccurred())
		Expect(st.SampleRate).To(Equal(1.0), "an org that never set one is sampled in full")
	})

	It("lists a destination whose credentials cannot be opened as unreadable and says so on its health", func(ctx SpecContext) {
		bad, err := svc.Create(ctx, org, actor, user, own, langfuse("bad"))
		Expect(err).NotTo(HaveOccurred())
		_, err = svc.Create(ctx, org, actor, user, own, langfuse("good"))
		Expect(err).NotTo(HaveOccurred())
		_, err = pool.Exec(ctx, `UPDATE trace_destinations SET credentials = '{"fmt":1}'::jsonb WHERE org_id = $1 AND id = $2`, org, bad.ID)
		Expect(err).NotTo(HaveOccurred())
		routes, err := svc.Routes(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(HaveLen(2))
		byName := map[string]tracedest.Route{}
		for _, r := range routes {
			byName[r.Name] = r
		}
		Expect(byName["good"].Unreadable).To(BeFalse())
		Expect(byName["bad"].Unreadable).To(BeTrue(), "listed, so it is not mistaken for a deleted destination")
		Expect(byName["bad"].Destination.Secrets).To(BeEmpty())
		got, _ := svc.Get(ctx, org, bad.ID)
		Expect(got.Health.LastError).To(ContainSubstring("cannot be opened"))
	})

	It("exports nothing to a destination whose credentials were lost, until they are entered again", func(ctx SpecContext) {
		d, err := svc.Create(ctx, org, actor, user, own, langfuse("lost"))
		Expect(err).NotTo(HaveOccurred())
		v0, err := svc.ConfigVersion(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		// What keys forget does (ADR 0085 decision 13): the envelope goes, the mark says since when.
		_, err = pool.Exec(ctx, `UPDATE trace_destinations SET credentials = NULL, credential_lost_at = now() WHERE org_id = $1 AND id = $2`, org, uuidOf(d.ID))
		Expect(err).NotTo(HaveOccurred())
		v1, _ := svc.ConfigVersion(ctx, org)
		Expect(v1).NotTo(Equal(v0), "the router drops what it queued for the destination")

		got, err := svc.Get(ctx, org, d.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.CredentialLostAt).NotTo(BeNil())
		Expect(got.SecretFields).To(ConsistOf("public_key", "secret_key"), "the form asks for these again")
		routes, err := svc.Routes(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(HaveLen(1))
		Expect(routes[0].Unreadable).To(BeTrue(), "listed, so it is not mistaken for a deleted destination")
		Expect(routes[0].Destination.Secrets).To(BeEmpty())
		ok, err := svc.WouldTrace(ctx, org, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "no destination can receive the run's spans")
		_, _, err = svc.Test(ctx, org, actor, d.ID)
		Expect(err).To(MatchError(tracedest.ErrCredentialLost))

		By("a change without secrets keeps it lost")
		in := langfuse("lost")
		in.Secrets = nil
		in.ResourceAttributes = map[string]string{"deployment.environment": "prod"}
		d2, err := svc.Update(ctx, org, actor, own, d.ID, nil, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(d2.CredentialLostAt).NotTo(BeNil())

		By("entering the secrets again tests them and ends the lost state")
		d3, err := svc.Update(ctx, org, actor, own, d.ID, nil, langfuse("lost"))
		Expect(err).NotTo(HaveOccurred())
		Expect(d3.CredentialLostAt).To(BeNil())
		routes, _ = svc.Routes(ctx, org)
		Expect(routes[0].Unreadable).To(BeFalse())
		Expect(routes[0].Destination.Secrets).To(HaveKey("secret_key"))
		Expect(got.Health.LastError).To(BeEmpty(), "the lost state is shown as itself, not as an export error")
	})

	It("sends no org's spans to a platform destination whose credentials were lost", func(ctx SpecContext) {
		_, err := svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("platform"))
		Expect(err).NotTo(HaveOccurred())
		v0, _ := svc.ConfigVersion(ctx, org)
		_, err = pool.Exec(ctx, `UPDATE platform_trace_destination SET credentials = NULL, credential_lost_at = now()`)
		Expect(err).NotTo(HaveOccurred())
		v1, _ := svc.ConfigVersion(ctx, org)
		Expect(v1).NotTo(Equal(v0))
		st, err := svc.GetSettings(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.HasPlatformDestination).To(BeFalse())
		routes, err := svc.Routes(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(HaveLen(1))
		Expect(routes[0].Platform).To(BeTrue())
		Expect(routes[0].Unreadable).To(BeTrue())
		_, _, err = svc.TestPlatform(ctx, auditor, actor)
		Expect(err).To(MatchError(tracedest.ErrCredentialLost))
		got, err := svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("platform"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.CredentialLostAt).To(BeNil())
		st, _ = svc.GetSettings(ctx, org)
		Expect(st.HasPlatformDestination).To(BeTrue())
	})

	It("tests a saved destination again and keeps the result, even when it fails", func(ctx SpecContext) {
		d, err := svc.Create(ctx, org, actor, user, own, langfuse("again"))
		Expect(err).NotTo(HaveOccurred())
		be.set(func(b *fakeBackend) { b.status = 500 })
		got, outcome, err := svc.Test(ctx, org, actor, d.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(outcome.OK).To(BeFalse())
		Expect(got.LastTest.OK).To(BeFalse())
		Expect(got.LastTest.Message).To(ContainSubstring("HTTP 500"))
		Expect(events(ctx, org, "trace.destination.test")).To(HaveLen(1))
	})

	It("deletes a destination and audits it", func(ctx SpecContext) {
		d, err := svc.Create(ctx, org, actor, user, own, langfuse("gone"))
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Delete(ctx, org, actor, d.ID)).To(Succeed())
		_, err = svc.Get(ctx, org, d.ID)
		Expect(err).To(MatchError(tracedest.ErrNotFound))
		Expect(events(ctx, org, "trace.destination.delete")).To(HaveLen(1))
	})

	Describe("settings", func() {
		It("defaults to level none, every run, nothing for the platform", func(ctx SpecContext) {
			st, err := svc.GetSettings(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.ContentLevel).To(Equal("none"))
			Expect(st.SampleRate).To(Equal(1.0))
			Expect(st.PlatformCopyLevel).To(Equal("none"))
			Expect(st.Ceiling).To(Equal("full"))
		})

		It("lets only an owner choose full, for the level and for the platform copy", func(ctx SpecContext) {
			_, err := svc.UpdateSettings(ctx, org, actor, adminOnly, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(1), PlatformCopyLevel: "none"})
			Expect(err).To(MatchError(tracedest.ErrOwnerOnly))
			_, err = svc.UpdateSettings(ctx, org, actor, adminOnly, tracedest.SettingsInput{ContentLevel: "redacted", SampleRate: rate(0.5), PlatformCopyLevel: "redacted"})
			Expect(err).NotTo(HaveOccurred(), "an admin may choose redacted")

			st, err := svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(0.5), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())
			Expect(st.ContentLevel).To(Equal("full"))
			Expect(st.SampleRate).To(Equal(0.5))

			By("an admin may lower it, and change sampling while it stays full")
			_, err = svc.UpdateSettings(ctx, org, actor, adminOnly, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(0.25), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.UpdateSettings(ctx, org, actor, adminOnly, tracedest.SettingsInput{ContentLevel: "none", SampleRate: rate(0.25), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())

			By("an owner may send the platform copy at full, but the copy never exceeds the org's own level")
			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(1), PlatformCopyLevel: "full"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.UpdateSettings(ctx, org, actor, adminOnly, tracedest.SettingsInput{ContentLevel: "redacted", SampleRate: rate(1), PlatformCopyLevel: "full"})
			Expect(err).To(HaveOccurred(), "the platform copy cannot exceed the org's level")

			Expect(events(ctx, org, "trace.settings.update")).To(HaveLen(5))
		})

		It("validates levels and the rate", func(ctx SpecContext) {
			for _, in := range []tracedest.SettingsInput{
				{ContentLevel: "all", SampleRate: rate(1), PlatformCopyLevel: "none"},
				{ContentLevel: "none", SampleRate: rate(1.5), PlatformCopyLevel: "none"},
				{ContentLevel: "none", SampleRate: rate(-0.1), PlatformCopyLevel: "none"},
				{ContentLevel: "none", SampleRate: rate(1), PlatformCopyLevel: "redacted"},
			} {
				_, err := svc.UpdateSettings(ctx, org, actor, own, in)
				var inv *tracedest.InvalidError
				Expect(err).To(BeAssignableToTypeOf(inv), fmt.Sprintf("%+v", in))
			}
		})

		It("holds the level to the platform ceiling, and lowering the ceiling lowers every org above it", func(ctx SpecContext) {
			other, otherUser, _ := pgtest.NewOrg(ctx, pool)
			low, lowUser, _ := pgtest.NewOrg(ctx, pool)
			_, err := svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(1), PlatformCopyLevel: "full"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.UpdateSettings(ctx, other, audit.User(otherUser), own, tracedest.SettingsInput{ContentLevel: "redacted", SampleRate: rate(1), PlatformCopyLevel: "redacted"})
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.UpdateSettings(ctx, low, audit.User(lowUser), own, tracedest.SettingsInput{ContentLevel: "none", SampleRate: rate(1), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())

			n, err := svc.SetPlatformCeiling(ctx, auditor, actor, "redacted")
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeNumerically(">=", 1), "the org at full was above the ceiling")
			st, _ := svc.GetSettings(ctx, org)
			Expect(st.ContentLevel).To(Equal("redacted"))
			Expect(st.PlatformCopyLevel).To(Equal("redacted"))
			Expect(st.Ceiling).To(Equal("redacted"))
			st, _ = svc.GetSettings(ctx, other)
			Expect(st.ContentLevel).To(Equal("redacted"))

			By("the lowered org sees why, and the platform admin's org has the change")
			ev := events(ctx, org, "trace.settings.lowered")
			Expect(ev).To(HaveLen(1))
			var summary map[string]any
			Expect(json.Unmarshal(ev[0].Summary, &summary)).To(Succeed())
			Expect(summary["before"]).To(HaveKeyWithValue("contentLevel", "full"))
			Expect(summary["after"]).To(HaveKeyWithValue("contentLevel", "redacted"))
			Expect(events(ctx, other, "trace.settings.lowered")).To(BeEmpty())
			Expect(events(ctx, auditor, "trace.platform_ceiling.set")).To(HaveLen(2), "the reset in BeforeEach and this change")

			By("no org can pick a level above it")
			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(1), PlatformCopyLevel: "none"})
			Expect(err).To(MatchError(tracedest.ErrAboveCeiling))

			By("the router sees the clamped level even for an org the ceiling had not reached yet")
			_, err = svc.Create(ctx, org, actor, user, own, langfuse("clamp"))
			Expect(err).NotTo(HaveOccurred())
			routes, _ := svc.Routes(ctx, org)
			Expect(routes[0].Level).To(Equal("redacted"))

			By("raising the ceiling again lowers nobody")
			n, err = svc.SetPlatformCeiling(ctx, auditor, actor, "full")
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())
			st, _ = svc.GetSettings(ctx, org)
			Expect(st.ContentLevel).To(Equal("redacted"))

			_, err = svc.SetPlatformCeiling(ctx, auditor, actor, "everything")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("the platform destination", func() {
		platform := func() tracedest.Input {
			in := langfuse("platform")
			in.Secrets = map[string]string{"public_key": "koinon_test_not-a-real-public", "secret_key": fakeKeyTwo}
			return in
		}
		It("is tested, sealed, audited in the admin's org, and receives the org's opt-in level", func(ctx SpecContext) {
			d, err := svc.SetPlatformDestination(ctx, auditor, actor, nil, platform())
			Expect(err).NotTo(HaveOccurred())
			Expect(d.LastTest.OK).To(BeTrue())
			got, err := svc.GetPlatform(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Destination).NotTo(BeNil())
			Expect(got.Destination.SecretFields).To(ConsistOf("public_key", "secret_key"))
			var raw []byte
			Expect(pool.QueryRow(ctx, `SELECT credentials FROM platform_trace_destination`).Scan(&raw)).To(Succeed())
			Expect(string(raw)).NotTo(ContainSubstring(fakeKeyTwo))

			st, _ := svc.GetSettings(ctx, org)
			Expect(st.HasPlatformDestination).To(BeTrue())

			By("an org without a destination of its own still has the platform one as a route, at level none")
			routes, err := svc.Routes(ctx, org)
			Expect(err).NotTo(HaveOccurred())
			Expect(routes).To(HaveLen(1))
			Expect(routes[0].Platform).To(BeTrue())
			Expect(routes[0].Level).To(Equal("none"))
			Expect(routes[0].Destination.Secrets).To(HaveKeyWithValue("secret_key", fakeKeyTwo))

			By("the org's opt-in raises the platform's copy, up to its own level")
			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "full", SampleRate: rate(1), PlatformCopyLevel: "redacted"})
			Expect(err).NotTo(HaveOccurred())
			routes, _ = svc.Routes(ctx, org)
			Expect(routes[0].Level).To(Equal("redacted"))

			ev := events(ctx, auditor, "trace.platform_destination.set")
			Expect(ev).To(HaveLen(1))
			Expect(string(ev[0].Summary)).NotTo(ContainSubstring(fakeKeyTwo))

			By("a failing test leaves it alone, and it can be deleted")
			be.set(func(b *fakeBackend) { b.status = 401 })
			_, err = svc.SetPlatformDestination(ctx, auditor, actor, nil, platform())
			var failed *tracedest.TestFailedError
			Expect(err).To(BeAssignableToTypeOf(failed))
			be.set(func(b *fakeBackend) { b.status = 200 })
			Expect(svc.DeletePlatformDestination(ctx, auditor, actor)).To(Succeed())
			Expect(svc.DeletePlatformDestination(ctx, auditor, actor)).To(MatchError(tracedest.ErrNotFound))
		})
	})

	Describe("the configuration fingerprint the router checks", func() {
		It("changes with the settings, the destinations and the ceiling, and not with health writes", func(ctx SpecContext) {
			v0, err := svc.ConfigVersion(ctx, org)
			Expect(err).NotTo(HaveOccurred())

			d, err := svc.Create(ctx, org, actor, user, own, langfuse("fp"))
			Expect(err).NotTo(HaveOccurred())
			v1, _ := svc.ConfigVersion(ctx, org)
			Expect(v1).NotTo(Equal(v0), "a new destination")

			_, err = pool.Exec(ctx, `UPDATE trace_destinations SET last_success_at = now(), last_error_at = now(), last_error = 'x', spans_dropped = spans_dropped + 5 WHERE org_id = $1 AND id = $2`, org, uuidOf(d.ID))
			Expect(err).NotTo(HaveOccurred())
			v2, _ := svc.ConfigVersion(ctx, org)
			Expect(v2).To(Equal(v1), "the router's own health writes do not invalidate its cache")

			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "redacted", SampleRate: rate(1), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())
			v3, _ := svc.ConfigVersion(ctx, org)
			Expect(v3).NotTo(Equal(v2), "the org's level")

			_, err = svc.SetPlatformCeiling(ctx, auditor, actor, "none")
			Expect(err).NotTo(HaveOccurred())
			v4, _ := svc.ConfigVersion(ctx, org)
			Expect(v4).NotTo(Equal(v3), "the platform ceiling")

			_, err = svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("plat-a"))
			Expect(err).NotTo(HaveOccurred())
			v5, _ := svc.ConfigVersion(ctx, org)
			Expect(svc.DeletePlatformDestination(ctx, auditor, actor)).To(Succeed())
			in := langfuse("plat-b")
			_, err = svc.SetPlatformDestination(ctx, auditor, actor, nil, in)
			Expect(err).NotTo(HaveOccurred())
			v6, _ := svc.ConfigVersion(ctx, org)
			Expect(v6).NotTo(Equal(v5), "delete and recreate resets config_version; the fingerprint must still change")
		})
	})

	Describe("the decision whether a run is traced", func() {
		It("traces nothing without a destination, every run with one, and a share when sampled", func(ctx SpecContext) {
			ok, err := svc.WouldTrace(ctx, org, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "no destination: nothing to trace for")

			_, err = svc.Create(ctx, org, actor, user, own, langfuse("sampled"))
			Expect(err).NotTo(HaveOccurred())
			ok, _ = svc.WouldTrace(ctx, org, 0.999)
			Expect(ok).To(BeTrue(), "the default is every run")

			_, err = svc.UpdateSettings(ctx, org, actor, own, tracedest.SettingsInput{ContentLevel: "none", SampleRate: rate(0.3), PlatformCopyLevel: "none"})
			Expect(err).NotTo(HaveOccurred())
			ok, _ = svc.WouldTrace(ctx, org, 0.29)
			Expect(ok).To(BeTrue())
			ok, _ = svc.WouldTrace(ctx, org, 0.31)
			Expect(ok).To(BeFalse())

			By("a platform destination alone makes an org's runs traceable")
			other, _, _ := pgtest.NewOrg(ctx, pool)
			ok, _ = svc.WouldTrace(ctx, other, 0)
			Expect(ok).To(BeFalse())
			_, err = svc.SetPlatformDestination(ctx, auditor, actor, nil, langfuse("platform"))
			Expect(err).NotTo(HaveOccurred())
			ok, _ = svc.WouldTrace(ctx, other, 0)
			Expect(ok).To(BeTrue())
		})
	})
})
