// SPDX-License-Identifier: Apache-2.0

package koinonagentd

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
)

func TestAgentd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-agentd")
}

const (
	db       = "postgres://koinon_test:not-a-real-password@localhost/koinon"
	endpoint = "http://koinon-api.koinon.svc"
)

var baseEnv = map[string]string{"KOINON_DATABASE_URL": db, "KOINON_OPENSHELL_ADDRESS": "o:1", "KOINON_GATEWAY_URL": "http://g:8080"}

// start runs koinon-agentd's startup, without its work, with the base environment plus env.
func start(env map[string]string, args ...string) error {
	all := map[string]string{}
	for k, v := range baseEnv {
		all[k] = v
	}
	for k, v := range env {
		all[k] = v
	}
	if args == nil {
		// nil would make cobra read os.Args, which carry the test runner's own flags.
		args = []string{}
	}
	return cli.Check(NewRootCommand(), args, all)
}

var _ = Describe("koinon-agentd's startup", func() {
	It("starts with its three required settings, from the environment or flags", func() {
		Expect(start(nil)).To(Succeed())
		Expect(cli.Check(NewRootCommand(), []string{"--database-url", db, "--openshell-address", "o:1", "--gateway-url", "http://g:8080"}, nil)).To(Succeed())
	})

	It("takes the database URL from DATABASE_URL and the owner from POD_NAME", func() {
		Expect(cli.Check(NewRootCommand(), []string{}, map[string]string{
			"DATABASE_URL": db, "POD_NAME": "koinon-agentd-0",
			"KOINON_OPENSHELL_ADDRESS": "o:1", "KOINON_GATEWAY_URL": "http://g:8080",
		})).To(Succeed())
	})

	It("names each missing required setting", func() {
		all := []string{"database-url", "openshell-address", "gateway-url"}
		for _, missing := range all {
			var args []string
			for _, f := range all {
				if f != missing {
					args = append(args, "--"+f+"=x:1")
				}
			}
			Expect(cli.Check(NewRootCommand(), args, nil)).To(MatchError(ContainSubstring("--"+missing)), missing)
		}
	})

	It("refuses an empty required setting from any source", func() {
		for env, flag := range map[string]string{"KOINON_DATABASE_URL": "database-url", "KOINON_OPENSHELL_ADDRESS": "openshell-address", "KOINON_GATEWAY_URL": "gateway-url"} {
			Expect(start(map[string]string{env: ""})).To(MatchError(ContainSubstring("--"+flag+" is required")), env)
			Expect(start(nil, "--"+flag+"=")).To(MatchError(ContainSubstring("--"+flag+" is required")), flag)
		}
	})

	It("refuses an unknown flag, an argument and a variable nothing reads", func() {
		Expect(start(nil, "--no-such-flag")).To(MatchError(ContainSubstring("unknown flag")))
		Expect(start(nil, "serve")).To(HaveOccurred())
		Expect(start(map[string]string{"KOINON_NO_SUCH": "1"})).To(MatchError(ContainSubstring("KOINON_NO_SUCH")))
	})

	It("requires the saved-work settings with checkpoints", func() {
		Expect(start(map[string]string{"KOINON_CHECKPOINTS": "true"})).To(MatchError(ContainSubstring("required with --checkpoints")))
		Expect(start(map[string]string{
			"KOINON_CHECKPOINTS": "true", "KOINON_SAVED_WORK_URL": "http://api:8083",
			"KOINON_SAVED_WORK_KEY_FILE": "/keys/saved-work",
		})).To(Succeed())
	})

	It("takes the drain deadline from a flag or the environment, and refuses one that does not parse (ADR 0085)", func() {
		Expect(start(map[string]string{"KOINON_DRAIN_DEADLINE": "2m"})).To(Succeed())
		Expect(start(nil, "--drain-deadline", "90s")).To(Succeed())
		Expect(start(map[string]string{"KOINON_DRAIN_DEADLINE": "soon"})).To(MatchError(ContainSubstring("KOINON_DRAIN_DEADLINE")))
	})
})

var _ = Describe("OTLP exporter settings", func() {
	tokenFile := func(content string) string {
		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte(content), 0o600)).To(Succeed())
		return p
	}
	otlp := func(endpoint, file string) map[string]string {
		env := map[string]string{}
		if endpoint != "" {
			env["KOINON_OTLP_ENDPOINT"] = endpoint
		}
		if file != "" {
			env["KOINON_OTLP_TOKEN_FILE"] = file
		}
		return env
	}

	It("is off by default and starts with the endpoint and a token file", func() {
		Expect(start(nil)).To(Succeed())
		// The startup check does not open the file: in the pod it is a mounted Secret.
		Expect(start(otlp(endpoint+":4318", "/secrets/otlp/token"))).To(Succeed())
		Expect(start(nil, "--otlp-endpoint", endpoint+":4318", "--otlp-token-file", "/secrets/otlp/token")).To(Succeed())
	})

	It("refuses one without the other and a bad endpoint", func() {
		tok := "/secrets/otlp/token"
		Expect(start(otlp(endpoint, ""))).To(MatchError(ContainSubstring("required together")))
		Expect(start(otlp("", tok))).To(MatchError(ContainSubstring("required together")))
		Expect(start(otlp("koinon-api", tok))).To(MatchError(ContainSubstring("http(s) URL")))
		Expect(start(otlp(endpoint+"/v1/traces", tok))).To(MatchError(ContainSubstring("no path")))
	})

	It("refuses a missing or invalid token file before serving, without printing the token", func() {
		Expect(readOTLPToken(tokenFile("koinon_test_not-a-real-token-agentd-0001\n"))).To(Succeed())
		Expect(readOTLPToken(filepath.Join(GinkgoT().TempDir(), "missing"))).To(MatchError(ContainSubstring("--otlp-token-file")))
		err := readOTLPToken(tokenFile("hunter2"))
		Expect(err).To(MatchError(ContainSubstring("at least")))
		Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
	})
})
