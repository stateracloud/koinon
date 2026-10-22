// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentd"
	"github.com/procoduck/koinon/internal/db/store"
)

var _ = DescribeTable("HarnessArgs",
	func(f store.AgentFlavor, config string, want []string) {
		Expect(agentd.HarnessArgs(f, []byte(config))).To(Equal(want))
	},
	Entry("plugin installs on: OpenCode loads plugins", store.AgentFlavorOpencode,
		`{"harness":{"pluginInstalls":true}}`, []string{"--plugin-installs"}),
	Entry("plugin installs off", store.AgentFlavorOpencode, `{"harness":{"pluginInstalls":false}}`, nil),
	Entry("no harness settings", store.AgentFlavorOpencode, `{"model":{"name":"m"}}`, nil),
	Entry("an unreadable config", store.AgentFlavorOpencode, `{"harness":{"pluginInstalls":"yes"}}`, nil),
	Entry("another flavor: the switch is OpenCode's", store.AgentFlavorHermes, `{"harness":{"pluginInstalls":true}}`, nil),
)
