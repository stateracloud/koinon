// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// The outbound clients take only a [mcpgateway.ScrubbedSource], which only [mcpgateway.CredentialScrub.Source]
// makes, so code that sends a credential the scrub did not record does not compile (#1221, ADR 0097).
var _ = Describe("the scrubbed credential type (#1221)", func() {
	It("does not compile code that sends a credential around the scrub", func(ctx SpecContext) {
		// The source is bypass.go.txt, not a .go file, so `go vet`, golangci-lint and the other tools that take
		// a changed directory do not try to compile it. An overlay gives it to `go build` as bypass.go.
		const dir = "testdata/bypass"
		src, err := filepath.Abs(filepath.Join(dir, "bypass.go.txt"))
		Expect(err).NotTo(HaveOccurred())
		want := wantLines(src)
		Expect(want).To(HaveLen(5), "each bypass in the package has a want marker")
		overlay := filepath.Join(GinkgoT().TempDir(), "overlay.json")
		raw, err := json.Marshal(map[string]map[string]string{"Replace": {strings.TrimSuffix(src, ".txt"): src}})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(overlay, raw, 0o600)).To(Succeed())

		goBin, err := exec.LookPath("go")
		Expect(err).NotTo(HaveOccurred(), "the go command builds the bypass package")
		cmd := exec.CommandContext(ctx, goBin, "build", "-overlay", overlay, "-o", os.DevNull, "./"+dir)
		out, err := cmd.CombinedOutput()
		Expect(err).To(HaveOccurred(), "the bypass package compiled:\n%s", out)
		var exit *exec.ExitError
		Expect(errors.As(err, &exit)).To(BeTrue(), "go build failed to run: %v", err)
		// Each line must fail with its own message: a guard that is gone (a renamed or deleted type) fails
		// with another error, and the spec catches that.
		for tag, w := range want {
			Expect(string(out)).To(MatchRegexp(`bypass\.go(\.txt)?:%d:\d+: [^\n]*%s`, w.line, regexp.QuoteMeta(w.msg)),
				"no compile error %q for %s:\n%s", w.msg, tag, out)
		}
	})

	It("fails closed with the zero value", func(ctx SpecContext) {
		var zero mcpgateway.ScrubbedSource
		Expect(zero.IsZero()).To(BeTrue())
		_, err := zero.Credential(ctx, gateway.MCPTarget{Name: "remote"}, gateway.AgentRef{Org: "acme"})
		Expect(err).To(MatchError(mcpgateway.ErrNoCredentialSource))

		_, err = mcpgateway.NewTargetHTTPClient(nil, gateway.MCPTarget{URL: "https://agent.example"}, gateway.AgentRef{Org: "acme"}, zero)
		Expect(err).To(MatchError(mcpgateway.ErrNoCredentialSource))

		_, err = a2aout.FetchCard(ctx, a2aout.Options{Target: gateway.MCPTarget{URL: "https://agent.example"}})
		Expect(err).To(MatchError(mcpgateway.ErrNoCredentialSource))
		_, err = a2aout.New(ctx, a2aout.Options{Target: gateway.MCPTarget{URL: "https://agent.example"}, CardHash: strings.Repeat("0", 64)})
		Expect(err).To(MatchError(mcpgateway.ErrNoCredentialSource))
	})

	It("gives the zero value for a nil source and records the values of a real one", func(ctx SpecContext) {
		sc := mcpgateway.NewCredentialScrub()
		Expect(sc.Source(nil).IsZero()).To(BeTrue())
		Expect(sc.Source(mcpgateway.ScrubbedSource{}).IsZero()).To(BeTrue())
		var none *mcpgateway.CredentialScrub
		Expect(none.Source(mcpgateway.StaticCredentials{}).IsZero()).To(BeTrue(), "a nil scrub records nothing, so it gives no source")

		src := sc.Source(mapCredentials{"remote": fakeToken})
		Expect(src.IsZero()).To(BeFalse())
		Expect(sc.Source(src)).To(Equal(src), "wrapping twice with the same scrub gives the same source")
		c, err := src.Credential(ctx, gateway.MCPTarget{Name: "remote"}, gateway.AgentRef{Org: "acme"})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Value).To(Equal(fakeToken))
		Expect(sc.TextFor("acme", fakeToken)).To(Equal("[redacted:secret]"))
	})
})

// wantMarker is one `// want: <tag> "<message>"` marker: the line must fail to compile with that message.
type wantMarker struct {
	line int
	msg  string
}

// wantLines gives each want marker in the file, by tag.
func wantLines(path string) map[string]wantMarker {
	GinkgoHelper()
	f, err := os.Open(path)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()
	marker := regexp.MustCompile(`// want: (\S+) "([^"]+)"$`)
	out := map[string]wantMarker{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		if m := marker.FindStringSubmatch(strings.TrimSpace(s.Text())); m != nil {
			Expect(out).NotTo(HaveKey(m[1]), fmt.Sprintf("tag %s is used twice", m[1]))
			out[m[1]] = wantMarker{line: n, msg: m[2]}
		}
	}
	Expect(s.Err()).NotTo(HaveOccurred())
	return out
}
