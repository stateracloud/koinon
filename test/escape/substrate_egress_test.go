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
// Koinon's own NetworkPolicies do not stand in for this: the egress pod is a Substrate pod, and a
// NetworkPolicy does not see its loopback traffic at all. The first line for the metadata address is
// the fence on the worker (charts/koinon/templates/substrate-networkpolicy.yaml, ADR 0120); this spec
// is the second line, the one the pinned release owns.
var _ = Describe("Substrate egress gateway", Label("escape", "substrate", "live"), Ordered, ContinueOnFailure, func() {
	var (
		name    string
		control ateapiv1.ControlClient
	)

	// Names that resolve to the refused addresses. A public wildcard DNS service answers them; set
	// KOINON_ESCAPE_EGRESS_REBIND to host:port names of your own when the cluster has no route to it.
	const defaultRebind = "169-254-169-254.sslip.io:80,127-0-0-1.sslip.io:80,127-0-0-1.sslip.io:15000,0-0-0-0.sslip.io:80"

	BeforeAll(func(ctx SpecContext) {
		useSubstrate("it sets an egress policy through the Substrate control API")
		c, closeConn := dialControl(os.Getenv)
		DeferCleanup(closeConn)
		control = c
		name = rig.sandbox(ctx, rig.workspace, fmt.Sprintf("kese-%d", suffix()))

		ref := &ateapiv1.ObjectRef{Atespace: rig.workspace, Name: name}
		_, err := control.CreateActorEgressPolicy(ctx, &ateapiv1.CreateActorEgressPolicyRequest{
			Actor: ref,
			EgressPolicy: &ateapiv1.EgressPolicy{
				Metadata: &ateapiv1.ResourceMetadata{Atespace: rig.workspace, Name: "default"},
				Rules: []*ateapiv1.EgressRule{{
					Http: &ateapiv1.HttpRule{
						// The allow-all rule: every name, on the ports the specs use.
						Hostnames: []string{"*"},
						Ports:     &ateapiv1.Ports{Numbers: []int32{80, 15000}},
					},
				}},
			},
		})
		Expect(err).NotTo(HaveOccurred(), "an egress policy with the `*` rule")
		DeferCleanup(func(ctx context.Context) error {
			_, err := control.DeleteActorEgressPolicy(ctx, &ateapiv1.DeleteActorEgressPolicyRequest{Actor: ref, Name: "default"})
			return err
		})
	}, NodeTimeout(8*time.Minute))

	It("lets the actor reach an ordinary name under the allow-all rule (the control of the specs below)", func(ctx SpecContext) {
		t := targets(envOr("KOINON_ESCAPE_EGRESS_CONTROL", "example.com:80"))[0]
		res := rig.probeIn(ctx, rig.workspace, name, check{"id": "control", "op": "http_status", "host": t.host, "port": t.port})
		Expect(res["control"]).To(HavePrefix("status "),
			"the `*` rule did not let %s through, so a denial below would mean nothing", t)
	}, NodeTimeout(2*time.Minute))

	It("refuses the metadata address, the egress pod's loopback and the Envoy admin port, also under the `*` rule", func(ctx SpecContext) {
		var checks []check
		for _, t := range targets(envOr("KOINON_ESCAPE_EGRESS_REBIND", defaultRebind)) {
			checks = append(checks, check{"id": "http " + t.String(), "op": "http_status", "host": t.host, "port": t.port})
		}
		res := rig.probeIn(ctx, rig.workspace, name, checks...)
		for _, c := range checks {
			got, _ := res[c["id"].(string)].(string)
			GinkgoWriter.Printf("%s -> %s\n", c["id"], got)
			// A refusal is a connection error, or an HTTP error the gateway answers itself (403 from
			// v1.0.1 for a denied address, 502 or 503 for a name that did not resolve). A 2xx or 3xx
			// is a response from the refused address.
			Expect(got).To(Or(BeElementOf(denial...), MatchRegexp(`^(status [45]\d\d|noresponse)$`)), c["id"])
		}
	}, NodeTimeout(racescale.D(3*time.Minute)))
})
