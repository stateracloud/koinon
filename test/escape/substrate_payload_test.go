// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// On Substrate a connect() proves nothing (issue #1515, ADR 0120 section 2.1, amendment of 2026-11-27).
// Each actor has its own network namespace. Its nftables rules send every TCP connect to a local tunnel
// that accepts the connection first, and then opens a CONNECT to atenet-egress. The egress layer decides
// on the Host or the SNI. So a deny shows only after the guest sends bytes: the tunnel closes the
// connection and no byte comes back. The specs below use the probe op "payload" (testdata/probe.py): it
// sends a TLS ClientHello or an HTTP request, and reports what came back.

// payloadDenials are the results of a payload probe that mean no byte came from the destination: the
// connect failed ("denial"), or the connection closed with no byte ("closed"). A result that is not here
// and is not a gateway refusal fails a spec, so a new way of failing is read before it counts as a denial.
var payloadDenials = append(slices.Clone(denial), "closed")

// gatewayRefusal is a refusal that atenet-egress sends as an HTTP answer (the 403 address_denied of its L7
// deny). The Envoy marker and the missing `x-envoy-upstream-service-time` header show that the answer is
// the gateway's own, and that no upstream was reached (as in http_status of the probe).
var gatewayRefusal = regexp.MustCompile(`^status (403|502|503) .* envoy=1 upstream=0$`)

// payloadDenied reports whether a payload result means that the destination sent no byte.
func payloadDenied(res any) bool {
	s, ok := res.(string)
	if !ok {
		return false
	}
	return slices.Contains(payloadDenials, any(s)) || gatewayRefusal.MatchString(s)
}

// httpPorts are the ports that get an HTTP request. Every other port gets a TLS ClientHello. Every deny
// target must be a service that answers bad input with a byte (a TLS server, an HTTP server, an SSH banner).
// A service that closes with no byte on bad input (CoreDNS on TCP 53, PostgreSQL) looks like a denial
// ("closed"): do not list such a port as a deny target.
var httpPorts = []int{80, 8080, 8099}

// payloadCheck is a check of the op "payload" for a target: an HTTP request on an HTTP port, else a
// TLS ClientHello with the host as its server name.
func payloadCheck(t target) check {
	kind := "tls"
	if slices.Contains(httpPorts, t.port) {
		kind = "http"
	}
	return check{"id": "payload " + t.String(), "op": "payload", "kind": kind, "host": t.host, "port": t.port}
}

// gatewayControl is the positive control of every deny spec below: the first gateway route, the one
// destination that the sandbox's egress policy allows (ADR 0120), answers an HTTP request with a status.
// If it does not, a probe that reads no byte proves nothing: the probe itself cannot read.
func gatewayControl() check {
	GinkgoHelper()
	routes, err := parseGatewayRoutes(os.Getenv("KOINON_ESCAPE_SUBSTRATE_GATEWAY_ROUTES"))
	Expect(err).NotTo(HaveOccurred())
	return check{"id": "control", "op": "payload", "kind": "http", "host": routes[0].Host, "port": int(routes[0].Port)}
}

// expectNoBytes sends bytes to every target and fails if any target sends bytes back. It runs the
// positive control in the same probe, reports every target (not only the first that fails), and writes
// each result to the Ginkgo log, the IPv6 targets too.
func expectNoBytes(ctx context.Context, probe probeFunc, ts []target) {
	GinkgoHelper()
	checks := []check{gatewayControl()}
	for _, t := range ts {
		checks = append(checks, payloadCheck(t))
	}
	res := probe(ctx, checks...)
	Expect(res["control"]).To(MatchRegexp(`^status \d{3} `),
		"control: the gateway route did not answer the probe's request, so a denial below would prove nothing")
	Expect(gatewayRefusal.MatchString(res["control"].(string))).To(BeFalse(),
		"control: the egress gateway refused the gateway route itself (%v): the route is not allowed, so a denial below would prove nothing", res["control"])
	var reached []string
	for _, c := range checks[1:] {
		id := c["id"].(string)
		GinkgoWriter.Printf("%s -> %v\n", id, res[id])
		if !payloadDenied(res[id]) {
			reached = append(reached, fmt.Sprintf("%s -> %v", id, res[id]))
		}
	}
	Expect(reached).To(BeEmpty(), "targets that answered the bytes (or gave a result that is not a known denial)")
}

// The classification above needs no cluster.
var _ = Describe("Substrate payload probe results", Label("unit"), func() {
	DescribeTable("tells a denial from an answer",
		func(res any, want bool) { Expect(payloadDenied(res)).To(Equal(want), fmt.Sprint(res)) },
		Entry("closed with no byte", "closed", true),
		Entry("reset", "ECONNRESET", true),
		Entry("refused", "ECONNREFUSED", true),
		Entry("no route to an IPv6 address", "ENETUNREACH", true),
		Entry("no byte in time", "timeout", true),
		Entry("the gateway's own 403", "status 403 server=envoy envoy=1 upstream=0", true),
		Entry("a 403 from an upstream", "status 403 server=nginx envoy=0 upstream=0", false),
		Entry("a 403 that Envoy proxied", "status 403 server=envoy envoy=1 upstream=1", false),
		Entry("a 200", "status 200 server=- envoy=0 upstream=0", false),
		Entry("TLS bytes", "bytes 7", false),
		Entry("a result that is not a string", float64(1), false),
		Entry("a new result", "ESOMETHINGNEW", false),
	)

	It("sends a TLS ClientHello to a TLS port and an HTTP request to an HTTP port", func() {
		c := payloadCheck(target{"10.96.0.1", 443})
		Expect(c).To(HaveKeyWithValue("kind", "tls"))
		Expect(c["id"]).To(Equal("payload 10.96.0.1:443"))
		Expect(payloadCheck(target{"example.com", 80})).To(HaveKeyWithValue("kind", "http"))
		Expect(payloadCheck(target{"2606:4700:4700::1111", 443})["id"]).To(HavePrefix("payload [2606:4700:4700::1111]:"))
	})

	It("keeps the probe's op table in step with the Go side", func() {
		Expect(probeScript).To(ContainSubstring(`"payload": payload`))
		Expect(strings.Count(probeScript, "def payload(")).To(Equal(1))
	})
})
