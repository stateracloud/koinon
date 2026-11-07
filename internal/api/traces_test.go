// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/stateracloud/koinon/internal/db/store"
	tracev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1/tracev1alpha1connect"
)

// reasonOf reads the google.rpc.ErrorInfo reason of a connect error ("" if it has none), and the
// TestResult detail if there is one.
func reasonOf(err error) (string, *tracev1alpha1.TestResult) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return "", nil
	}
	var reason string
	var result *tracev1alpha1.TestResult
	for _, d := range ce.Details() {
		msg, derr := connectproto.UnmarshalErrorDetail(d)
		if derr != nil {
			continue
		}
		switch m := msg.(type) {
		case *errdetails.ErrorInfo:
			Expect(m.GetDomain()).To(Equal("koinon.dev"))
			reason = m.GetReason()
		case *tracev1alpha1.TestResult:
			result = m
		}
	}
	return reason, result
}

func traceClient(c *client) tracev1alpha1connect.TraceDestinationServiceClient {
	return tracev1alpha1connect.NewTraceDestinationServiceClient(connect.NewClient(connecthttp.NewTransport(c.b.http, c.b.e.srv.URL)))
}

var _ = Describe("TraceDestinationService", func() {
	const fakeKey = "koinon_test_not-a-real-token"
	var (
		e      *env
		org    string
		owner  *client
		status atomic.Int32
		hits   atomic.Int32
		otlp   *httptest.Server
	)
	BeforeEach(func(ctx SpecContext) {
		status.Store(200)
		hits.Store(0)
		otlp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			hits.Add(1)
			w.WriteHeader(int(status.Load()))
		}))
		DeferCleanup(otlp.Close)
		e = newEnv(ctx, envOptions{allowedHosts: []string{"127.0.0.1"}})
		org, owner = e.newTeamOrg(ctx)
	})
	input := func(name string) *tracev1alpha1.DestinationInput {
		return &tracev1alpha1.DestinationInput{
			Name: name, Backend: tracev1alpha1.Backend_BACKEND_DATADOG, EndpointUrl: otlp.URL + "/v1/traces", Enabled: true,
			Secrets: map[string]string{"api_key": fakeKey},
		}
	}
	setPlatformAdmin := func(ctx SpecContext, c *client) {
		_, err := e.pool.Exec(ctx, `INSERT INTO platform_admins (user_id) VALUES ($1)`, c.userID)
		Expect(err).NotTo(HaveOccurred())
	}

	It("lets admins manage destinations, keeps members out, tests before saving and never returns a secret", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		member := e.userIn(ctx, org, store.OrgRoleMember)

		_, err := traceClient(member).ListDestinations(ctx, &tracev1alpha1.ListDestinationsRequest{OrgId: org})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal("ADMIN_REQUIRED"))
		_, err = traceClient(member).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("m")})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(hits.Load()).To(BeZero(), "a refused caller triggers no test trace")

		By("a backend that refuses the test trace saves nothing")
		status.Store(401)
		_, err = traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("dd")})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("HTTP 401"))
		reason, result := reasonOf(err)
		Expect(reason).To(Equal("TEST_FAILED"))
		Expect(result.GetMessage()).To(Equal("the backend answered HTTP 401 (the credentials were refused)"), "the outcome alone, no prefix")
		Expect(err.Error()).NotTo(ContainSubstring(fakeKey))
		list, err := traceClient(admin).ListDestinations(ctx, &tracev1alpha1.ListDestinationsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetDestinations()).To(BeEmpty())

		By("a backend that accepts it lets the destination be saved")
		status.Store(200)
		created, err := traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("dd")})
		Expect(err).NotTo(HaveOccurred())
		d := created.GetDestination()
		Expect(d.GetLastTest().GetOk()).To(BeTrue())
		Expect(d.GetSecretFields()).To(ConsistOf("api_key"))
		Expect(strings.Contains(d.String(), fakeKey)).To(BeFalse())

		list, err = traceClient(admin).ListDestinations(ctx, &tracev1alpha1.ListDestinationsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetDestinations()).To(HaveLen(1))
		Expect(strings.Contains(list.String(), fakeKey)).To(BeFalse())

		_, err = traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("dd")})
		Expect(codeOf(err)).To(Equal(connect.CodeAlreadyExists))
		bad := input("bad")
		bad.Backend = tracev1alpha1.Backend_BACKEND_UNSPECIFIED
		_, err = traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: bad})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))

		tested, err := traceClient(admin).TestDestination(ctx, &tracev1alpha1.TestDestinationRequest{OrgId: org, DestinationId: d.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(tested.GetResult().GetOk()).To(BeTrue())

		_, err = traceClient(admin).DeleteDestination(ctx, &tracev1alpha1.DeleteDestinationRequest{OrgId: org, DestinationId: d.GetId()})
		Expect(err).NotTo(HaveOccurred())

		By("every change is in the audit log, without the key")
		rows, err := e.pool.Query(ctx, `SELECT action, summary::text FROM audit_events WHERE org_id = $1 AND action LIKE 'trace.%' ORDER BY chain_seq`, org)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var actions []string
		for rows.Next() {
			var a, sum string
			Expect(rows.Scan(&a, &sum)).To(Succeed())
			Expect(sum).NotTo(ContainSubstring(fakeKey))
			actions = append(actions, a)
		}
		Expect(actions).To(Equal([]string{"trace.destination.create", "trace.destination.test", "trace.destination.delete"}))
	})

	It("lets only an owner set the content level full", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		_, err := traceClient(admin).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL, SampleRate: new(float64(1)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal("OWNER_ONLY"))
		res, err := traceClient(admin).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED, SampleRate: new(float64(0.5)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetSettings().GetContentLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED))
		Expect(res.GetSettings().GetSampleRate()).To(Equal(0.5))

		res, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL, SampleRate: new(float64(0.5)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.GetSettings().GetContentLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL))
		got, err := traceClient(admin).GetSettings(ctx, &tracev1alpha1.GetSettingsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetSettings().GetContentLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL))

		_, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{OrgId: org, SampleRate: new(float64(1))})
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument), "a level is required")
	})

	It("keeps the platform calls for platform admins, and lowering the ceiling lowers the orgs above it", func(ctx SpecContext) {
		_, err := traceClient(owner).GetPlatform(ctx, &tracev1alpha1.GetPlatformRequest{})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied), "an org owner is not a platform admin")
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal("PLATFORM_ADMIN_REQUIRED"))
		_, err = traceClient(owner).SetPlatformCeiling(ctx, &tracev1alpha1.SetPlatformCeilingRequest{Ceiling: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		_, err = traceClient(owner).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: input("x")})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(hits.Load()).To(BeZero())

		_, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL, SampleRate: new(float64(1)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(err).NotTo(HaveOccurred())

		pa := e.userIn(ctx, org, store.OrgRoleMember)
		setPlatformAdmin(ctx, pa)
		DeferCleanup(func(ctx SpecContext) {
			_, _ = traceClient(pa).SetPlatformCeiling(ctx, &tracev1alpha1.SetPlatformCeilingRequest{Ceiling: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL})
			_, _ = e.pool.Exec(ctx, `DELETE FROM platform_trace_destination`)
		})

		set, err := traceClient(pa).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: input("ignored")})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetDestination().GetSecretFields()).To(ConsistOf("api_key"))
		got, err := traceClient(pa).GetPlatform(ctx, &tracev1alpha1.GetPlatformRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetDestination().GetEndpointUrl()).To(Equal(otlp.URL + "/v1/traces"))
		Expect(strings.Contains(got.String(), fakeKey)).To(BeFalse())

		By("a stale platform form is refused with ABORTED")
		stale := got.GetDestination().GetConfigVersion()
		quiet := input("ignored")
		quiet.Secrets, quiet.Enabled = nil, false
		_, err = traceClient(pa).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: quiet, ExpectedVersion: new(stale)})
		Expect(err).NotTo(HaveOccurred())
		_, err = traceClient(pa).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: quiet, ExpectedVersion: new(stale)})
		Expect(codeOf(err)).To(Equal(connect.CodeAborted))
		_, err = traceClient(pa).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: input("ignored")})
		Expect(err).NotTo(HaveOccurred())

		By("a failing test keeps the platform destination as it was")
		status.Store(500)
		_, err = traceClient(pa).SetPlatformDestination(ctx, &tracev1alpha1.SetPlatformDestinationRequest{Destination: input("ignored")})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		status.Store(200)

		lowered, err := traceClient(pa).SetPlatformCeiling(ctx, &tracev1alpha1.SetPlatformCeilingRequest{Ceiling: tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED})
		Expect(err).NotTo(HaveOccurred())
		Expect(lowered.GetOrgsLowered()).To(BeNumerically(">=", 1))
		s, err := traceClient(owner).GetSettings(ctx, &tracev1alpha1.GetSettingsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(s.GetSettings().GetContentLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED))
		Expect(s.GetSettings().GetCeiling()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED))
		Expect(s.GetSettings().GetHasPlatformDestination()).To(BeTrue())

		By("the owner cannot raise the level above the ceiling")
		_, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL, SampleRate: new(float64(1)), PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))

		By("the lowering is in the org's audit log, pointing at the platform change, which is in the install chain")
		var n int
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN install_audit_events i ON i.id::text = a.summary->>'install_event_id'
			WHERE a.org_id = $1 AND a.action = 'trace.settings.lowered' AND i.action = 'trace.platform_ceiling.set'`, org).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events i JOIN users u ON u.id::text = i.actor_id
			WHERE u.id = $1 AND i.action IN ('trace.platform_destination.set', 'trace.platform_ceiling.set')`, pa.userID).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(4), "three destination saves and the ceiling")
		Expect(e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN users u ON u.personal_org_id = a.org_id
			WHERE u.id = $1 AND a.action LIKE 'trace.platform_%'`, pa.userID).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "nothing in the admin's personal org")
	})

	It("does not let a platform admin choose what an org sends: they may lower its settings but not raise them", func(ctx SpecContext) {
		_, otherOwner := e.newTeamOrg(ctx)
		pa := otherOwner // a platform admin with no role in `org`
		setPlatformAdmin(ctx, pa)
		raise := func(level, copyLevel tracev1alpha1.ContentLevel) error {
			_, err := traceClient(pa).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{OrgId: org, ContentLevel: level, PlatformCopyLevel: copyLevel})
			return err
		}
		none, redacted, full := tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE, tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED, tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL
		err := raise(full, none)
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal("NOT_MEMBER"))
		Expect(codeOf(raise(redacted, none))).To(Equal(connect.CodePermissionDenied))
		Expect(codeOf(raise(redacted, redacted))).To(Equal(connect.CodePermissionDenied))

		By("the org's owner sets it, then the platform admin lowers it, leaving the stored rate alone")
		_, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: full, SampleRate: new(float64(0.3)), PlatformCopyLevel: redacted,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(raise(none, none)).To(Succeed())
		got, err := traceClient(owner).GetSettings(ctx, &tracev1alpha1.GetSettingsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetSettings().GetContentLevel()).To(Equal(none))
		Expect(got.GetSettings().GetSampleRate()).To(Equal(0.3))
	})

	It("refuses a stale form with ABORTED and a non-owner's new destination while the level is full", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		created, err := traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("dd")})
		Expect(err).NotTo(HaveOccurred())
		d := created.GetDestination()
		Expect(d.GetConfigVersion()).To(BeNumerically(">=", 1))

		in := input("dd")
		in.Secrets = nil
		in.Enabled = false
		first, err := traceClient(admin).UpdateDestination(ctx, &tracev1alpha1.UpdateDestinationRequest{
			OrgId: org, DestinationId: d.GetId(), Destination: in, ExpectedVersion: new(d.GetConfigVersion()),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(first.GetDestination().GetConfigVersion()).To(Equal(d.GetConfigVersion() + 1))
		_, err = traceClient(admin).UpdateDestination(ctx, &tracev1alpha1.UpdateDestinationRequest{
			OrgId: org, DestinationId: d.GetId(), Destination: in, ExpectedVersion: new(d.GetConfigVersion()),
		})
		Expect(codeOf(err)).To(Equal(connect.CodeAborted))
		reason, _ := reasonOf(err)
		Expect(reason).To(Equal("CHANGED"))

		_, err = traceClient(owner).UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_FULL, PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("second")})
		Expect(codeOf(err)).To(Equal(connect.CodePermissionDenied))
		reason, _ = reasonOf(err)
		Expect(reason).To(Equal("OWNER_AT_FULL"))
		_, err = traceClient(owner).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("second")})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decides once whether a run is traced", func(ctx SpecContext) {
		admin := e.userIn(ctx, org, store.OrgRoleAdmin)
		a := owner.activeAgent(ctx, org)

		_, r := owner.createTicket(ctx, org, a.GetId(), "untraced")
		traced, err := e.traces.DecideRun(ctx, uuid(org), uuid(r.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(BeFalse(), "an org without a destination traces nothing")

		_, err = traceClient(admin).CreateDestination(ctx, &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: input("dd")})
		Expect(err).NotTo(HaveOccurred())
		traced, err = e.traces.DecideRun(ctx, uuid(org), uuid(r.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(BeFalse(), "the run was decided before the destination existed and keeps its decision")

		a2 := owner.activeAgent(ctx, org)
		_, r2 := owner.createTicket(ctx, org, a2.GetId(), "traced")
		traced, err = e.traces.DecideRun(ctx, uuid(org), uuid(r2.GetId()))
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(BeTrue(), "the default samples every run")
		var stored *bool
		Expect(e.pool.QueryRow(ctx, `SELECT traced FROM runs WHERE org_id = $1 AND id = $2`, org, r2.GetId()).Scan(&stored)).To(Succeed())
		Expect(stored).NotTo(BeNil())
		Expect(*stored).To(BeTrue())
	})
})
