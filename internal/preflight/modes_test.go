// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	"flag"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/project"
)

// The modes the preflight and the reconciler read from flags and config files.
var _ = Describe("install modes", func() {
	It("parses the chart's gateway and Postgres modes from flags and refuses others", func() {
		gw, pg := project.GatewayStandalone, project.PostgresCNPG
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.Var(&gw, "gateway-mode", "")
		fs.Var(&pg, "postgres-mode", "")
		Expect(fs.Parse([]string{"--gateway-mode=kubernetes", "--postgres-mode=external"})).To(Succeed())
		Expect(gw).To(Equal(project.GatewayKubernetes))
		Expect(pg).To(Equal(project.PostgresExternal))

		Expect(gw.Set("k8s")).To(MatchError(`gateway mode "k8s" is neither standalone nor kubernetes`))
		Expect(pg.Set("")).To(MatchError(`postgres mode "" is neither cnpg nor external`))
		Expect(gw).To(Equal(project.GatewayKubernetes), "a refused value changes nothing")
	})

	It("parses them from YAML", func() {
		var v struct {
			Gateway project.GatewayMode `yaml:"gateway"`
		}
		Expect(yaml.Unmarshal([]byte("gateway: standalone"), &v)).To(Succeed())
		Expect(v.Gateway).To(Equal(project.GatewayStandalone))
		Expect(yaml.Unmarshal([]byte("gateway: istio"), &v)).To(MatchError(ContainSubstring(`gateway mode "istio"`)))
	})
})
