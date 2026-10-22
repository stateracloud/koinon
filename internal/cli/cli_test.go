// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"flag"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/cli"
)

func TestCLI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CLI Suite")
}

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

var _ = Describe("BindEnv", func() {
	var (
		fs      *flag.FlagSet
		tlsDir  string
		workers int
		lease   time.Duration
	)
	BeforeEach(func() {
		fs = flag.NewFlagSet("t", flag.ContinueOnError)
		fs.StringVar(&tlsDir, "openshell-tls-dir", "", "")
		fs.IntVar(&workers, "workers", 8, "")
		fs.DurationVar(&lease, "lease", time.Second, "")
	})

	It("names a flag's variable KOINON_ and the flag in upper case with underscores", func() {
		Expect(cli.EnvName("openshell-tls-dir")).To(Equal("KOINON_OPENSHELL_TLS_DIR"))
	})

	It("sets flags from the environment, and command-line flags win", func() {
		Expect(cli.BindEnv(fs, env(map[string]string{
			"KOINON_OPENSHELL_TLS_DIR": "/tls", "KOINON_WORKERS": "3", "WORKERS": "99",
		}))).To(Succeed())
		Expect(fs.Parse([]string{"--workers=4"})).To(Succeed())
		Expect(tlsDir).To(Equal("/tls"))
		Expect(workers).To(Equal(4))
		Expect(lease).To(Equal(time.Second), "unset variables keep the default")
	})

	It("names the variable whose value does not parse", func() {
		err := cli.BindEnv(fs, env(map[string]string{"KOINON_WORKERS": "many", "KOINON_LEASE": "soon"}))
		Expect(err).To(MatchError(ContainSubstring("KOINON_LEASE")))
	})

	It("reads the process environment without a getenv", func() {
		GinkgoT().Setenv("KOINON_WORKERS", "5")
		Expect(cli.BindEnv(fs, nil)).To(Succeed())
		Expect(workers).To(Equal(5))
	})
})

var _ = Describe("OpenShell.Dial", func() {
	It("connects without TLS when no TLS directory is set", func() {
		b, err := cli.OpenShell{Address: "127.0.0.1:1"}.Dial()
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Close()).To(Succeed())
	})

	It("loads the client bundle from the TLS directory", func() {
		dir := GinkgoT().TempDir()
		_, err := cli.OpenShell{Address: "127.0.0.1:1", TLSDir: dir}.Dial()
		Expect(err).To(MatchError(ContainSubstring(filepath.Join(dir, "tls.crt"))))
	})
})
