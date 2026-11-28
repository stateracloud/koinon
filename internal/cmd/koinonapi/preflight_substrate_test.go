// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/preflight"
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

	It("runs the Substrate checks by default since #1093, and the OpenShell checks with --driver=openshell", func() {
		c, err := parse("--substrate-probes=false", "--netpol-probe=false")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.check.Substrate).NotTo(BeNil())
		c, err = parse("--driver=openshell", "--seccomp-probe=false", "--netpol-probe=false")
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

	// Issue #1176: the probe pods run outside Substrate's namespaces, whose admission policies refuse a bare pod.
	It("runs the probe pods in a namespace of their own, and refuses a Substrate namespace for them", func() {
		c, err := parse("--driver=substrate", "--probe-image=img", "--netpol-probe=false")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.check.Substrate.ProbeNamespace).To(Equal(preflight.DefaultSubstrateProbeNamespace))
		c, err = parse("--driver=substrate", "--probe-image=img", "--netpol-probe=false", "--substrate-probe-namespace=probes")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.check.Substrate.ProbeNamespace).To(Equal("probes"))
		for _, args := range [][]string{
			{"--substrate-probe-namespace=ate-system"},
			{"--substrate-probe-namespace=ate-workers"},
			{"--substrate-namespace=sub", "--substrate-probe-namespace=sub"},
			{"--substrate-worker-namespace=wk", "--substrate-probe-namespace=wk"},
		} {
			_, err := parse(append([]string{"--driver=substrate", "--probe-image=img", "--netpol-probe=false"}, args...)...)
			Expect(err).To(MatchError(ContainSubstring("is a Substrate namespace")), "%v", args)
		}
	})

	It("gives the Substrate prober the probe namespace, not a Substrate namespace", func() {
		for _, args := range [][]string{nil, {"--substrate-probe-namespace=probes"}} {
			c, err := parse(append([]string{"--driver=substrate", "--probe-image=img", "--netpol-probe=false"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
			p := substrateProber(*c, fake.NewClientset(), *c.check.Substrate)
			Expect(p.Namespace).To(Equal(c.check.Substrate.ProbeNamespace))
			Expect(p.Namespace).NotTo(BeElementOf("ate-system", "ate-workers"))
			Expect(p.Image).To(Equal("img"))
		}
	})

	Describe("substrate install", func() {
		run := func(args ...string) error {
			cmd := newSubstrateCommand()
			cmd.SetArgs(append([]string{"install", "--substrate-source=/x", "--storage-bucket=b", "--probe-image=img"}, args...))
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			return cmd.Execute()
		}

		It("needs a kubeconfig and a context, never the ambient one", func() {
			Expect(run("--kubeconfig=/k")).To(MatchError(ContainSubstring("--kubeconfig and --context")))
			Expect(run("--context=lab")).To(MatchError(ContainSubstring("--kubeconfig and --context")))
		})

		It("refuses to run with the probes off", func() {
			Expect(run("--kubeconfig=/k", "--context=lab", "--substrate-probes=false")).To(MatchError(ContainSubstring("--substrate-probes=false")))
		})

		It("refuses the preflight-only issuer flag, so the probe and ate-setup cannot use different issuers", func() {
			Expect(run("--kubeconfig=/k", "--context=lab", "--substrate-expected-issuer=https://kubernetes.default.svc")).
				To(MatchError(ContainSubstring("--expected-jwt-issuer")))
		})

		It("takes --platform generic, gke or kind, and refuses another value", func() {
			Expect(run("--kubeconfig=/k", "--context=lab", "--platform=talos")).To(MatchError(ContainSubstring(`unknown platform "talos" (valid: generic, gke, kind)`)))
			for _, p := range []string{"generic", "gke", "kind"} {
				// A valid platform passes the flag checks; the run then stops at the kubeconfig that does not exist.
				err := run("--kubeconfig=/nonexistent", "--context=lab", "--platform="+p)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).NotTo(ContainSubstring("platform"))
			}
		})

		It("keeps --kind as a deprecated alias of --platform kind and refuses a conflict", func() {
			err := run("--kubeconfig=/nonexistent", "--context=lab", "--kind")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("platform"))
			Expect(run("--kubeconfig=/k", "--context=lab", "--kind", "--platform=generic")).To(MatchError(ContainSubstring("--kind means --platform kind and conflicts with --platform generic")))
		})
	})
})
