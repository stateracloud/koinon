// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// bearerFile sends the contents of a file as a bearer token, read again on every call.
type bearerFile struct{ path string }

func (b bearerFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(raw))}, nil
}

func (bearerFile) RequireTransportSecurity() bool { return true }

// dialControl opens a client of Substrate's control API with the settings of the rig. The Koinon
// driver has no egress API (the gateway owns egress, ADR 0097), so the spec sets the actor's egress
// policy the way an operator does: through the control API.
func dialControl(get func(string) string) (ateapiv1.ControlClient, func()) {
	GinkgoHelper()
	tlsCfg := &tls.Config{ServerName: get("KOINON_ESCAPE_SUBSTRATE_SERVER_NAME"), MinVersion: tls.VersionTLS12}
	if caFile := get("KOINON_ESCAPE_SUBSTRATE_CA"); caFile != "" {
		ca, err := os.ReadFile(caFile)
		Expect(err).NotTo(HaveOccurred())
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca)).To(BeTrue(), "no certificates in "+caFile)
		tlsCfg.RootCAs = pool
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
	if tokenFile := get("KOINON_ESCAPE_SUBSTRATE_TOKEN_FILE"); tokenFile != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearerFile{path: tokenFile}))
	}
	conn, err := grpc.NewClient(get("KOINON_ESCAPE_SUBSTRATE_CONTROL"), opts...)
	Expect(err).NotTo(HaveOccurred())
	return ateapiv1.NewControlClient(conn), func() { _ = conn.Close() }
}

// Substrate v1.0.1 (issue #1363, upstream #3881): the egress gateway refuses to connect to a loopback,
// link-local or unspecified address under any rule, `*` included, after it resolves the name. On
// v1.0.0 an actor with a `*` rule could reach the cloud metadata server and the Envoy admin port of
// the egress pod (127.0.0.1:15000, atenet-egress.yaml) through a public name that resolves there.
//
// What Koinon's own NetworkPolicies cover. The control-plane fence
// (charts/koinon/templates/substrate-networkpolicy.yaml, the `-substrate-control` policy and the
// default-deny egress policy) gives atenet-egress no route to any external address, 169.254.169.254
// included, on a CNI that enforces NetworkPolicy. So the metadata address is already blocked there;
// this spec is the second line, the one the pinned release owns. The egress pod's own loopback and
// the Envoy admin port are not covered by any NetworkPolicy (loopback traffic is not seen by one),
// so only the v1.0.1 fix stops those.
//
// Precondition. The control spec needs atenet-egress to reach KOINON_ESCAPE_EGRESS_CONTROL. On a rig
// with the chart policies on, add that destination to workerNetworkPolicy.extraEgress or run the
// rig with the chart policies off; without it the control fails and the specs below prove nothing.
//
// How the probe reaches the gateway. The probe connects to the address of the control name (an
// address that leaves the guest and that the `*` rule sends through atenet-egress) and puts the
// refused name in the Host header, so the gateway does the resolution. A name the guest would
// resolve to its own loopback is never connected to from the guest.
var _ = Describe("Substrate egress gateway", Label("escape", "substrate", "live"), Ordered, ContinueOnFailure, func() {
	var (
		name    string
		control ateapiv1.ControlClient
	)

	// Names that resolve to the refused addresses. A public wildcard DNS service answers them; set
	// KOINON_ESCAPE_EGRESS_REBIND to host:port names of your own when the cluster has no route to it.
	const defaultControl = "example.com:80"
	const defaultRebind = "169-254-169-254.sslip.io:80,127-0-0-1.sslip.io:80,127-0-0-1.sslip.io:15000,0-0-0-0.sslip.io:80"

	BeforeAll(func(ctx SpecContext) {
		useSubstrate("it sets an egress policy through the Substrate control API")
		c, closeConn := dialControl(os.Getenv)
		DeferCleanup(closeConn)
		control = c
		name = rig.sandbox(ctx, rig.workspace, fmt.Sprintf("kese-%d", suffix()))

		ref := &ateapiv1.ObjectRef{Atespace: rig.workspace, Name: name}
		// Since #1444 the driver writes the policy "default" (the gateway routes only) when it creates the
		// sandbox, so the spec replaces its rules; a Create would fail with AlreadyExists (issue #1508).
		allowAll := []*ateapiv1.EgressRule{{
			Http: &ateapiv1.HttpRule{
				// The allow-all rule: every name, on the ports the specs use.
				Hostnames: []string{"*"},
				Ports:     &ateapiv1.Ports{Numbers: []int32{80, 15000}},
			},
		}}
		cur, err := control.GetActorEgressPolicy(ctx, &ateapiv1.GetActorEgressPolicyRequest{Actor: ref, Name: "default"})
		Expect(err).NotTo(HaveOccurred(), "the gateway-only policy that the driver wrote")
		upd := &ateapiv1.EgressPolicy{
			Metadata: &ateapiv1.ResourceMetadata{
				Atespace: rig.workspace, Name: "default",
				Uid: cur.GetMetadata().GetUid(), Version: cur.GetMetadata().GetVersion(),
			},
			Rules: allowAll,
		}
		_, err = control.UpdateActorEgressPolicy(ctx, &ateapiv1.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: upd})
		Expect(err).NotTo(HaveOccurred(), "an egress policy with the `*` rule")
		DeferCleanup(func(ctx context.Context) error {
			_, err := control.DeleteActorEgressPolicy(ctx, &ateapiv1.DeleteActorEgressPolicyRequest{Actor: ref, Name: "default"})
			return err
		})
	}, NodeTimeout(8*time.Minute))

	It("lets the actor reach an ordinary name under the allow-all rule (the control of the specs below)", func(ctx SpecContext) {
		t := targets(envOr("KOINON_ESCAPE_EGRESS_CONTROL", defaultControl))[0]
		res := rig.probeIn(ctx, rig.workspace, name, check{"id": "control", "op": "http_status", "host": t.host, "via": t.host, "port": t.port})
		Expect(res["control"]).To(MatchRegexp(`^status [23]\d\d `),
			"the `*` rule did not let %s through (does atenet-egress have a route to it? see the precondition above and docs/dev/escape-substrate.md), so a denial below would mean nothing", t)
	}, NodeTimeout(2*time.Minute))

	It("refuses the metadata address, the egress pod's loopback and the Envoy admin port, also under the `*` rule", func(ctx SpecContext) {
		var checks []check
		via := targets(envOr("KOINON_ESCAPE_EGRESS_CONTROL", defaultControl))[0].host
		for _, t := range targets(envOr("KOINON_ESCAPE_EGRESS_REBIND", defaultRebind)) {
			checks = append(checks, check{"id": "http " + t.String(), "op": "http_status", "host": t.host, "via": via, "port": t.port})
		}
		res := rig.probeIn(ctx, rig.workspace, name, checks...)
		for _, c := range checks {
			got, _ := res[c["id"].(string)].(string)
			GinkgoWriter.Printf("%s -> %s\n", c["id"], got)
			// A refusal is the gateway's own: a reset it sends, or 403, 502 or 503 with an Envoy marker
			// (the `server` or an `x-envoy-*` header) and no `x-envoy-upstream-service-time`, which Envoy adds
			// to every answer it proxied from an upstream (upstream=1: the destination was reached). The probe
			// sends the headers that GCE and Azure metadata servers need, so a server that is reached
			// answers 200, not 403. A 401 or 400 is no refusal: a metadata server
			// that is reached answers that to a bare GET (AWS IMDSv2, Azure IMDS). "badvia" and
			// "ECONNREFUSED" mean the connection never reached the gateway, so they prove nothing.
			Expect(got).To(Or(Equal("ECONNRESET"), MatchRegexp(`^status (403|502|503) .* envoy=1 upstream=0$`)), c["id"])
		}
	}, NodeTimeout(racescale.D(3*time.Minute)))
})
