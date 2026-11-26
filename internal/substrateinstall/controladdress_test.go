// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

// The ateapi certificate that ate-setup makes names api.ate-system.svc only. The reconciler dials the control
// address with TLS verification, so a value with .cluster.local fails the handshake and no run leaves "starting"
// (issue #1091, kind leg). The files that carry the address for the kind route must use the name of the
// certificate.

import (
	"os"
	"regexp"
	"testing"

	. "github.com/onsi/gomega"
)

func TestKindValuesDialTheNameOfTheAteapiCertificate(t *testing.T) {
	line := regexp.MustCompile(`(?m)^\s*controlAddress:\s*(\S+)\s*$`)
	for _, file := range []string{
		"../../test/e2e/kind/substrate-values.yaml",
		"../../charts/koinon/ci/substrate-values.yaml",
	} {
		g := NewWithT(t)
		body, err := os.ReadFile(file)
		g.Expect(err).NotTo(HaveOccurred())
		m := line.FindSubmatch(body)
		g.Expect(m).NotTo(BeNil(), file)
		g.Expect(string(m[1])).To(Equal("api.ate-system.svc:443"), file)
		// ate-setup makes ateapi accept this audience only (auth.go, apiAudience); another one gives "invalid bearer token".
		aud := regexp.MustCompile(`(?m)^\s*tokenAudience:\s*(\S+)\s*$`).FindSubmatch(body)
		g.Expect(aud).NotTo(BeNil(), file)
		g.Expect(string(aud[1])).To(Equal("api.ate-system.svc"), file)
	}
}
