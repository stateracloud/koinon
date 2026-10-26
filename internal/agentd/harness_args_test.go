// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/db/store"
)

var _ = DescribeTable("HarnessArgs",
	func(f store.AgentFlavor, config string, want []string) {
		Expect(agentd.HarnessArgs(f, []byte(config))).To(Equal(want))
	},
	Entry("plugin installs on: OpenCode loads plugins", store.AgentFlavorOpencode,
		`{"harness":{"pluginInstalls":true}}`, []string{"--plugin-installs"}),
	Entry("plugin installs off", store.AgentFlavorOpencode, `{"harness":{"pluginInstalls":false}}`, nil),
	Entry("no harness settings", store.AgentFlavorOpencode, `{"model":"m"}`, nil),
	Entry("an unreadable config", store.AgentFlavorOpencode, `{"harness":{"pluginInstalls":"yes"}}`, nil),
	Entry("a config agentconfig.Parse cannot read", store.AgentFlavorOpencode, `{"harness":{"pluginInstalls":true},"model":{"name":"m"}}`, nil),
	Entry("a flavor without the option: the switch is OpenCode's", store.AgentFlavor("other"), `{"harness":{"pluginInstalls":true}}`, nil),
)

var _ = DescribeTable("VersionModel",
	func(config, want string) {
		Expect(agentd.VersionModel([]byte(config))).To(Equal(want))
	},
	Entry("a model, as agentconfig.FromTemplate writes it", `{"model":"fake/model-1"}`, "fake/model-1"),
	Entry("padded", `{"model":" fake/model-1 "}`, "fake/model-1"),
	Entry("default: the install default", `{"model":"default"}`, ""),
	Entry("no model: the install default", `{"instructions":"help"}`, ""),
	Entry("no config", ``, ""),
	Entry("an unreadable config: the install default", `{"model":{"name":"fake/model-1"}}`, ""),
)
