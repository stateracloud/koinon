// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/harness"
)

var _ = DescribeTable("HarnessArgs",
	func(f string, config string, want []string) {
		Expect(agentd.HarnessArgs(f, []byte(config))).To(Equal(want))
	},
	Entry("plugin installs on: OpenCode loads plugins", harness.OpenCode,
		`{"harness":{"pluginInstalls":true}}`, []string{"--plugin-installs", "--no-web-tools"}),
	Entry("plugin installs off", harness.OpenCode, `{"harness":{"pluginInstalls":false}}`, []string{"--no-web-tools"}),
	Entry("no harness settings: no web egress, so no built-in web tools", harness.OpenCode, `{"model":"m"}`, []string{"--no-web-tools"}),
	Entry("an unreadable config", harness.OpenCode, `{"harness":{"pluginInstalls":"yes"}}`, []string{"--no-web-tools"}),
	Entry("a config agentconfig.Parse cannot read", harness.OpenCode, `{"harness":{"pluginInstalls":true},"model":{"name":"m"}}`, []string{"--no-web-tools"}),
	Entry("a harness without the option: the switch is OpenCode's", string("other"), `{"harness":{"pluginInstalls":true}}`, nil),
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
