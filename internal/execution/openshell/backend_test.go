// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
	"github.com/stateracloud/koinon/internal/project"
)

var (
	orgLabel   = project.Domain + "/org"
	agentLabel = project.Domain + "/agent"
)

func agentgatewayProvider(revision string) execution.Provider {
	return execution.Provider{
		Name: "agentgateway-key",
		Profile: execution.ProviderProfile{
			ID:       "koinon-agentgateway",
			Category: execution.CategoryInference,
			Credentials: []execution.ProfileCredential{{
				Name: "api_key", EnvVars: []string{"OPENAI_API_KEY"}, Required: true,
				AuthStyle: "bearer", HeaderName: "authorization",
			}},
			Endpoints: []execution.Endpoint{{
				Host: "agentgateway.koinon.svc", Port: 8080, Protocol: "rest",
				Allow: []execution.L7Rule{{Method: "POST", Path: "/v1/chat/completions"}},
			}},
			Binaries: []string{"/usr/local/bin/opencode"},
		},
		Credentials:        map[string]string{"OPENAI_API_KEY": "key-" + revision},
		CredentialRevision: revision,
		Config:             map[string]string{"base_url": "http://agentgateway.koinon.svc:8080"},
	}
}

func harnessPolicy() execution.Policy {
	return execution.Policy{
		Filesystem: execution.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/usr", "/etc/koinon"},
			ReadWrite:      []string{"/sandbox", "/tmp"},
		},
		Network: map[string]execution.NetworkRule{
			"git": {
				Endpoints: []execution.Endpoint{{
					Host: "github.com", Port: 443, Protocol: "rest", Access: execution.AccessReadOnly,
					Deny: []execution.L7Rule{{Method: "POST", Path: "/**"}},
				}},
				Binaries: []string{"/usr/bin/git"},
			},
			"dns-free-l4": {
				Endpoints: []execution.Endpoint{{Host: "proxy.internal", Port: 3128}},
				Binaries:  []string{"/usr/bin/curl"},
			},
		},
	}
}

var _ = Describe("Backend", func() {
	var (
		b  *openshell.Backend
		gw *fakegateway.Gateway
	)

	BeforeEach(func() {
		b, gw = newBackend()
	})

	Describe("EnsureWorkspace", func() {
		It("creates the workspace once", func(ctx SpecContext) {
			w, err := b.EnsureWorkspace(ctx, "org-beta", map[string]string{orgLabel: "beta"})
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Name).To(Equal("org-beta"))
			Expect(w.Active).To(BeTrue())
			Expect(w.ID).NotTo(BeEmpty())
			Expect(w.Labels).To(HaveKeyWithValue(orgLabel, "beta"))

			again, err := b.EnsureWorkspace(ctx, "org-beta", map[string]string{orgLabel: "beta"})
			Expect(err).NotTo(HaveOccurred())
			Expect(again.ID).To(Equal(w.ID))
			Expect(gw.Calls("CreateWorkspace")).To(Equal(2)) // ws from newBackend, org-beta once
		})

		It("survives a concurrent create", func(ctx SpecContext) {
			gw.FailNext("CreateWorkspace", status.Error(codes.AlreadyExists, "raced"))
			w, err := b.EnsureWorkspace(ctx, "org-gamma", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Name).To(Equal("org-gamma"))
		})

		It("reports an invalid name", func(ctx SpecContext) {
			_, err := b.EnsureWorkspace(ctx, "Not_A_Label", nil)
			Expect(err).To(MatchError(execution.ErrInvalid))
		})
	})

	Describe("EnsureProvider", func() {
		It("creates the profile with stable placeholders and the provider with its credentials", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())

			profile := gw.Profile(ws, "koinon-agentgateway")
			Expect(profile).NotTo(BeNil())
			Expect(profile.GetCredentials()).To(HaveLen(1))
			Expect(profile.GetCredentials()[0].GetStablePlaceholder()).To(BeTrue())
			Expect(profile.GetEndpoints()[0].GetEnforcement()).To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE))
			Expect(profile.GetAnnotations()).To(HaveKey(openshell.SpecHashAnnotation))

			p := gw.Provider(ws, "agentgateway-key")
			Expect(p.GetType()).To(Equal("koinon-agentgateway"))
			Expect(p.GetProfileWorkspace()).To(Equal(ws))
			Expect(p.GetCredentials()).To(Equal(map[string]string{"OPENAI_API_KEY": "key-1"}))
			Expect(p.GetConfig()).To(HaveKeyWithValue(openshell.CredentialRevisionConfig, "1"))
		})

		It("writes nothing when the records already match", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())

			Expect(gw.Calls("ImportProviderProfiles")).To(Equal(1))
			Expect(gw.Calls("UpdateProviderProfiles")).To(Equal(0))
			Expect(gw.Calls("LintProviderProfiles")).To(Equal(1))
			Expect(gw.Calls("CreateProvider")).To(Equal(1))
			Expect(gw.Calls("UpdateProvider")).To(Equal(0))
		})

		It("pushes credentials only when the revision changes", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())

			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("2"))).To(Succeed())
			Expect(gw.Provider(ws, "agentgateway-key").GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", "key-2"))
			Expect(gw.Calls("UpdateProvider")).To(Equal(1))

			// Same revision, different value in hand: not pushed.
			same := agentgatewayProvider("2")
			same.Credentials["OPENAI_API_KEY"] = "ignored"
			Expect(b.EnsureProvider(ctx, ws, same)).To(Succeed())
			Expect(gw.Provider(ws, "agentgateway-key").GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", "key-2"))
			Expect(gw.Calls("UpdateProvider")).To(Equal(1))
		})

		It("updates and removes config keys without touching credentials", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())
			p := agentgatewayProvider("1")
			p.Config = map[string]string{"model": "deepseek"}
			Expect(b.EnsureProvider(ctx, ws, p)).To(Succeed())

			stored := gw.Provider(ws, "agentgateway-key")
			Expect(stored.GetConfig()).To(Equal(map[string]string{"model": "deepseek", openshell.CredentialRevisionConfig: "1"}))
			Expect(stored.GetCredentials()).To(HaveKeyWithValue("OPENAI_API_KEY", "key-1"))
		})

		It("updates a changed profile with its resource version", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())
			p := agentgatewayProvider("1")
			p.Profile.Binaries = append(p.Profile.Binaries, "/usr/local/bin/extra")
			Expect(b.EnsureProvider(ctx, ws, p)).To(Succeed())

			Expect(gw.Calls("UpdateProviderProfiles")).To(Equal(1))
			profile := gw.Profile(ws, "koinon-agentgateway")
			Expect(profile.GetResourceVersion()).To(Equal(uint64(2)))
			Expect(profile.GetBinaries()).To(HaveLen(2))

			Expect(b.EnsureProvider(ctx, ws, p)).To(Succeed())
			Expect(gw.Calls("UpdateProviderProfiles")).To(Equal(1))
		})

		It("refuses a gateway that would drop stable placeholders", func(ctx SpecContext) {
			gw.NoStablePlaceholder = true
			err := b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))
			Expect(err).To(MatchError(execution.ErrUnsupported))
			Expect(gw.Calls("ImportProviderProfiles")).To(Equal(0))
		})

		It("returns the gateway's diagnostics for an invalid profile", func(ctx SpecContext) {
			p := agentgatewayProvider("1")
			p.Profile.Endpoints[0].Allow = nil
			err := b.EnsureProvider(ctx, ws, p)
			Expect(err).To(MatchError(execution.ErrInvalid))
			Expect(err.Error()).To(ContainSubstring("protocol requires rules or access"))
		})

		It("reports a provider of another type as a conflict", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())
			p := agentgatewayProvider("1")
			p.Profile.ID = "koinon-other"
			Expect(b.EnsureProvider(ctx, ws, p)).To(MatchError(execution.ErrConflict))
		})

		It("deletes idempotently", func(ctx SpecContext) {
			Expect(b.EnsureProvider(ctx, ws, agentgatewayProvider("1"))).To(Succeed())
			Expect(b.DeleteProvider(ctx, ws, "agentgateway-key")).To(Succeed())
			Expect(b.DeleteProvider(ctx, ws, "agentgateway-key")).To(Succeed())
			Expect(gw.Provider(ws, "agentgateway-key")).To(BeNil())
		})
	})

	Describe("sandbox lifecycle", func() {
		It("renders only the resource values that are set", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "limits-only", Image: "img", MemoryLimit: "1Gi"})
			Expect(err).NotTo(HaveOccurred())
			Expect(gw.Sandbox(ws, "limits-only").GetSpec().GetTemplate().GetResources().AsMap()).To(Equal(map[string]any{
				"limits": map[string]any{"memory": "1Gi"},
			}))
			_, err = b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "none", Image: "img"})
			Expect(err).NotTo(HaveOccurred())
			Expect(gw.Sandbox(ws, "none").GetSpec().GetTemplate().GetResources()).To(BeNil())
		})

		spec := func() execution.SandboxSpec {
			return execution.SandboxSpec{
				Name: "agent-1", Image: "ghcr.io/stateracloud/koinon/sandbox:dev",
				Command: []string{"sleep", "infinity"}, Policy: harnessPolicy(),
				Labels: map[string]string{agentLabel: "a1"}, CPURequest: "250m", CPULimit: "1", MemoryRequest: "1Gi", MemoryLimit: "2Gi",
			}
		}

		It("renders the hardening defaults into the create request", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())

			stored := gw.Sandbox(ws, "agent-1").GetSpec()
			Expect(stored.GetEnvironment()).To(BeEmpty())
			Expect(stored.GetTemplate().GetEnvironment()).To(BeEmpty())
			Expect(stored.GetTemplate().GetImage()).To(Equal("ghcr.io/stateracloud/koinon/sandbox:dev"))
			Expect(stored.GetTemplate().GetResources().AsMap()).To(Equal(map[string]any{
				"requests": map[string]any{"cpu": "250m", "memory": "1Gi"},
				"limits":   map[string]any{"cpu": "1", "memory": "2Gi"},
			}))

			policy := stored.GetPolicy()
			Expect(policy.GetLandlock().GetCompatibility()).To(Equal("hard_requirement"))
			git := policy.GetNetworkPolicies()["git"].GetEndpoints()[0]
			Expect(git.GetEnforcement()).To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE))
			Expect(git.GetTls()).To(Equal(sandboxv1.NetworkTlsMode_NETWORK_TLS_MODE_UNSPECIFIED))
			Expect(git.GetAllowUninspectedCredentials()).To(BeFalse())
			Expect(git.GetDenyRules()).To(HaveLen(1))
			l4 := policy.GetNetworkPolicies()["dns-free-l4"].GetEndpoints()[0]
			Expect(l4.GetEnforcement()).To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_UNSPECIFIED))
		})

		It("rejects L7 rules on an endpoint without a protocol", func(ctx SpecContext) {
			s := spec()
			s.Policy.Network["bad"] = execution.NetworkRule{Endpoints: []execution.Endpoint{{
				Host: "example.com", Port: 443, Access: execution.AccessFull,
			}}}
			_, err := b.CreateSandbox(ctx, ws, s)
			Expect(err).To(MatchError(execution.ErrInvalid))
			Expect(gw.Calls("CreateSandbox")).To(Equal(0))
		})

		It("creates, stops, starts and deletes idempotently", func(ctx SpecContext) {
			sb, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseReady))
			Expect(sb.Labels).To(HaveKeyWithValue(agentLabel, "a1"))

			again, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())
			Expect(again.ID).To(Equal(sb.ID))

			for range 2 {
				sb, err = b.StopSandbox(ctx, ws, "agent-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(sb.Phase).To(Equal(execution.PhaseStopped))
			}
			for range 2 {
				sb, err = b.StartSandbox(ctx, ws, "agent-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(sb.Phase).To(Equal(execution.PhaseReady))
			}
			Expect(deleteErr(b.DeleteSandbox(ctx, ws, "agent-1"))).To(Succeed())
			Expect(deleteErr(b.DeleteSandbox(ctx, ws, "agent-1"))).To(Succeed())

			_, err = b.GetSandbox(ctx, ws, "agent-1")
			Expect(err).To(MatchError(execution.ErrNotFound))
		})

		It("does not hide a start that fails for another reason", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())
			gw.SetPhase(ws, "agent-1", openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR)
			_, err = b.StartSandbox(ctx, ws, "agent-1")
			Expect(err).To(MatchError(execution.ErrFailedPrecondition))
		})

		It("refuses a wildcard egress host in a policy", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())
			before := len(gw.Policies(ws, "agent-1"))

			for _, host := range []string{"*.example.com", "*", "api.*.example.com"} {
				p := harnessPolicy()
				p.Network["wild"] = execution.NetworkRule{
					Endpoints: []execution.Endpoint{{Host: host, Port: 443, Protocol: "rest", Access: execution.AccessReadOnly}},
					Binaries:  []string{"/usr/bin/curl"},
				}
				_, err = b.EnsurePolicy(ctx, ws, "agent-1", p)
				Expect(err).To(MatchError(ContainSubstring("DNS exfiltration")), host)
				Expect(gw.Policies(ws, "agent-1")).To(HaveLen(before))
			}
		})

		It("keeps the policy version for an unchanged policy and bumps it for new network rules", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())

			v, err := b.EnsurePolicy(ctx, ws, "agent-1", harnessPolicy())
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(uint32(1)))

			p := harnessPolicy()
			p.Network["echo"] = execution.NetworkRule{
				Endpoints: []execution.Endpoint{{Host: "echo.internal", Port: 443, Protocol: "rest", Access: execution.AccessReadOnly}},
				Binaries:  []string{"/usr/bin/curl"},
			}
			v, err = b.EnsurePolicy(ctx, ws, "agent-1", p)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(uint32(2)))
			v, err = b.EnsurePolicy(ctx, ws, "agent-1", p)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(uint32(2)))

			revs := gw.Policies(ws, "agent-1")
			Expect(revs).To(HaveLen(2))
			Expect(revs[1].GetNetworkPolicies()["echo"].GetEndpoints()[0].GetEnforcement()).
				To(Equal(sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE))
		})

		It("reports a filesystem change on a running sandbox as invalid", func(ctx SpecContext) {
			_, err := b.CreateSandbox(ctx, ws, spec())
			Expect(err).NotTo(HaveOccurred())
			p := harnessPolicy()
			p.Filesystem.ReadWrite = append(p.Filesystem.ReadWrite, "/opt")
			_, err = b.EnsurePolicy(ctx, ws, "agent-1", p)
			Expect(err).To(MatchError(execution.ErrInvalid))
		})
	})

	DescribeTable("maps gateway errors",
		func(ctx SpecContext, code codes.Code, kind error) {
			gw.FailNext("GetSandbox", status.Error(code, "boom"))
			_, err := b.GetSandbox(ctx, ws, "x")

			Expect(err).To(MatchError(kind))
			var e *execution.Error
			Expect(errors.As(err, &e)).To(BeTrue())
			Expect(e.Op).To(Equal("GetSandbox"))
			Expect(status.Code(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring("boom"))
		},
		Entry("not found", codes.NotFound, execution.ErrNotFound),
		Entry("already exists", codes.AlreadyExists, execution.ErrAlreadyExists),
		Entry("aborted", codes.Aborted, execution.ErrConflict),
		Entry("invalid argument", codes.InvalidArgument, execution.ErrInvalid),
		Entry("failed precondition", codes.FailedPrecondition, execution.ErrFailedPrecondition),
		Entry("permission denied", codes.PermissionDenied, execution.ErrPermissionDenied),
		Entry("unauthenticated", codes.Unauthenticated, execution.ErrPermissionDenied),
		Entry("unavailable", codes.Unavailable, execution.ErrUnavailable),
		Entry("resource exhausted", codes.ResourceExhausted, execution.ErrUnavailable),
		Entry("unimplemented", codes.Unimplemented, execution.ErrUnsupported),
		Entry("deadline", codes.DeadlineExceeded, context.DeadlineExceeded),
		Entry("canceled", codes.Canceled, context.Canceled),
	)

	It("keeps unknown gateway errors without a kind", func(ctx SpecContext) {
		gw.FailNext("GetSandbox", status.Error(codes.Internal, "kaput"))
		_, err := b.GetSandbox(ctx, ws, "x")
		Expect(status.Code(err)).To(Equal(codes.Internal))
		Expect(errors.Is(err, execution.ErrUnavailable)).To(BeFalse())
	})
})

// deleteErr is the error of a DeleteSandbox call, for the specs that check only that.
func deleteErr(_ execution.DeleteOutcome, err error) error { return err }

var _ = Describe("DeleteSandbox outcome", func() {
	It("maps the gateway's outcome", func(ctx SpecContext) {
		b, _ := newBackend()
		_, err := b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "agent-1", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		out, err := b.DeleteSandbox(ctx, ws, "agent-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(execution.DeleteCompleted))
		out, err = b.DeleteSandbox(ctx, ws, "agent-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(execution.DeleteAbsent))
	})

	It("counts a response without an outcome as accepted", func(ctx SpecContext) {
		gw := fakegateway.New()
		conn, err := gw.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(gw.Stop)
		DeferCleanup(conn.Close)
		b := openshell.New(noOutcomeConn{conn})
		_, err = b.EnsureWorkspace(ctx, ws, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "agent-1", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		out, err := b.DeleteSandbox(ctx, ws, "agent-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(execution.DeleteAccepted))
	})
})

// noOutcomeConn answers DeleteSandbox with the gateway's reply minus its outcome.
type noOutcomeConn struct{ *grpc.ClientConn }

func (c noOutcomeConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	err := c.ClientConn.Invoke(ctx, method, args, reply, opts...)
	if resp, ok := reply.(*openshellv1.DeleteSandboxResponse); ok {
		resp.Outcome = openshellv1.DeletionOutcome_DELETION_OUTCOME_UNSPECIFIED
	}
	return err
}
