// SPDX-License-Identifier: Apache-2.0

package e2eclient_test

import (
	"context"
	"net/http"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/test/e2e/e2eclient"
)

func TestE2EClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "e2eclient")
}

var _ = Describe("SetToolTier", func() {
	It("reads the tool hash and sends it with the tier", func() {
		var putPath, putBody string
		admin := func(_ context.Context, method, path string, body ...string) (int, []byte, error) {
			if method == http.MethodGet {
				return 200, []byte(`{"tools":[{"name":"read_item","toolHash":"h1"},{"name":"write_item","toolHash":"h2"}]}`), nil
			}
			putPath, putBody = path, body[0]
			return 200, nil, nil
		}
		Expect(e2eclient.SetToolTier(context.Background(), admin, "srv", "write_item", "ask")).To(Succeed())
		Expect(putPath).To(Equal("/mcp-servers/srv/tools/write_item/tier"))
		Expect(putBody).To(MatchJSON(`{"tier":"ask","toolHash":"h2"}`))
	})

	It("fails when the tool is not listed", func() {
		admin := func(context.Context, string, string, ...string) (int, []byte, error) {
			return 200, []byte(`{"tools":[]}`), nil
		}
		Expect(e2eclient.SetToolTier(context.Background(), admin, "srv", "write_item", "ask")).To(MatchError(ContainSubstring("write_item")))
	})

	It("fails on a refused decision", func() {
		admin := func(_ context.Context, method, _ string, _ ...string) (int, []byte, error) {
			if method == http.MethodGet {
				return 200, []byte(`{"tools":[{"name":"write_item","toolHash":"h"}]}`), nil
			}
			return 409, []byte("stale"), nil
		}
		Expect(e2eclient.SetToolTier(context.Background(), admin, "srv", "write_item", "ask")).To(MatchError(ContainSubstring("409")))
	})
})
