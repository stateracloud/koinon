// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"
)

var _ = Describe("preflight --driver substrate", func() {
	parse := func(args ...string) (*preflightConfig, error) {
		var c preflightConfig
		fs := pflag.NewFlagSet("preflight", pflag.ContinueOnError)
		c.flags(fs)
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		return &c, c.validate()
	}

	It("keeps openshell as the default driver", func() {
		c, err := parse("--seccomp-probe=false", "--netpol-probe=false")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.check.Substrate).To(BeNil())
	})

	It("selects the Substrate checks, turns the Kata seccomp probe off and reads pools and selectors", func() {
		c, err := parse("--driver=substrate", "--substrate-probes=false", "--netpol-probe=false",
			"--substrate-worker-pool=vm=pool-vm", "--substrate-node-selector=koinon.dev/pool=workers")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.check.Substrate.WorkerPools).To(Equal(map[string]string{"vm": "pool-vm"}))
		Expect(c.check.Substrate.NodeSelector).To(Equal(map[string]string{"koinon.dev/pool": "workers"}))
		Expect(c.seccompProbe).To(BeFalse())
	})

	It("refuses a bad driver, a bad pool and probes without an image", func() {
		_, err := parse("--driver=kata")
		Expect(err).To(MatchError(ContainSubstring("openshell or substrate")))
		_, err = parse("--driver=substrate", "--substrate-worker-pool=vm", "--netpol-probe=false")
		Expect(err).To(MatchError(ContainSubstring("<tier>=<name>")))
		_, err = parse("--driver=substrate", "--netpol-probe=false")
		Expect(err).To(MatchError(ContainSubstring("--substrate-probes needs --probe-image")))
	})
})
