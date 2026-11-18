// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
)

var _ = Describe("Execution", func() {
	It("defaults to OpenShell, which needs its address", func() {
		e := cli.Execution{}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--openshell-address")))
		Expect(e.Validate("gw:8080")).To(Succeed())
		Expect(e.IsSubstrate()).To(BeFalse())
	})
	It("needs both Substrate addresses and no OpenShell address for substrate", func() {
		e := cli.Execution{Driver: cli.DriverSubstrate}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-control-address")))
		e.Substrate = cli.Substrate{ControlAddress: "api:443", RouterAddress: "router:80"}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-allow-unhardened")))
		e.Substrate.AllowUnhardened = true
		Expect(e.Validate("")).To(Succeed())
	})
	It("refuses an unknown driver", func() {
		Expect((&cli.Execution{Driver: "nomad"}).Validate("x")).To(MatchError(ContainSubstring("nomad")))
	})
	It("dials substrate lazily and exposes its collectors", func() {
		c, err := cli.Execution{Driver: cli.DriverSubstrate, Substrate: cli.Substrate{ControlAddress: "127.0.0.1:1", RouterAddress: "127.0.0.1:2", AllowUnhardened: true}}.Dial()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(c.Close)
		Expect(c.Backend).NotTo(BeNil())
		Expect(c.Collectors).NotTo(BeEmpty())
	})
})
