// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	"github.com/stateracloud/koinon/internal/agentd"
)

var _ = DescribeTable("the workspace check of permission requests",
	func(path string, inside bool) {
		Expect(agentd.Within("/sandbox/work", path)).To(Equal(inside))
	},
	Entry("the directory", "/sandbox/work", true),
	Entry("a file in it", "/sandbox/work/a/b.txt", true),
	Entry("a relative path", "a/b.txt", true),
	Entry("a trailing slash", "/sandbox/work/", true),
	Entry("the home", "/sandbox/.koinon/AGENTS.md", false),
	Entry("a sibling with the same prefix", "/sandbox/workshop/x", false),
	Entry("dot-dot out of it", "/sandbox/work/../.koinon/AGENTS.md", false),
	Entry("a relative escape", "../x", false),
	Entry("the root", "/", false),
	Entry("system files", "/etc/passwd", false),
)

var _ = Describe("the options agentd answers a permission request with", func() {
	read := acp.ToolKindRead
	req := func(kind acp.ToolKind, path string, opts ...acp.PermissionOptionKind) acp.PermissionRequest {
		r := acp.PermissionRequest{ToolCall: sdk.ToolCallUpdate{Kind: &kind}}
		if path != "" {
			r.ToolCall.Locations = []sdk.ToolCallLocation{{Path: path}}
		}
		for i, k := range opts {
			r.Options = append(r.Options, acp.PermissionOption{OptionId: sdk.PermissionOptionId(fmt.Sprint("o", i)), Kind: k})
		}
		return r
	}
	decide := func(r acp.PermissionRequest) (agentd.Verdict, string) {
		v, id, _ := agentd.Decide(agentd.DefaultPermissions(), r, "/sandbox/work", nil)
		return v, id
	}

	It("allows once when a once option is offered, even after an always option", func() {
		v, id := decide(req(read, "", acp.AllowAlways, acp.AllowOnce))
		Expect(v).To(Equal(agentd.Allow))
		Expect(id).To(Equal("o1"))
	})
	It("never chooses allow-always: with only that, it cancels", func() {
		v, id := decide(req(read, "", acp.AllowAlways, acp.RejectOnce))
		Expect(v).To(Equal(agentd.Reject))
		Expect(id).To(Equal("o1"), "the reject-once option")
		v, id = decide(req(read, "", acp.AllowAlways))
		Expect(v).To(Equal(agentd.Reject))
		Expect(id).To(BeEmpty(), "a cancel")
	})
	It("never chooses reject-always: with only that, it cancels", func() {
		v, id := decide(req(acp.ToolKindFetch, "", acp.AllowOnce, acp.RejectAlways))
		Expect(v).To(Equal(agentd.Reject))
		Expect(id).To(BeEmpty(), "a cancel")
	})
})

var _ = Describe("the workspace check of permission requests with links", func() {
	read := acp.ToolKindRead
	req := func(path string) acp.PermissionRequest {
		return acp.PermissionRequest{
			ToolCall: sdk.ToolCallUpdate{Kind: &read, Locations: []sdk.ToolCallLocation{{Path: path}}},
			Options:  []acp.PermissionOption{{OptionId: "yes", Kind: acp.AllowOnce}, {OptionId: "no", Kind: acp.RejectOnce}},
		}
	}
	resolver := func(table map[string]string, err error) agentd.PathResolver {
		return func(ws string, paths []string) ([]string, error) {
			if err != nil {
				return nil, err
			}
			out := []string{ws}
			for _, p := range paths {
				if r, ok := table[p]; ok {
					out = append(out, r)
				} else {
					out = append(out, p)
				}
			}
			return out, nil
		}
	}
	decide := func(path string, r agentd.PathResolver) (agentd.Verdict, string, string) {
		return agentd.Decide(agentd.DefaultPermissions(), req(path), "/sandbox/work", r)
	}

	It("allows a path that resolves inside the workspace", func() {
		v, id, _ := decide("/sandbox/work/a.txt", resolver(nil, nil))
		Expect(v).To(Equal(agentd.Allow))
		Expect(id).To(Equal("yes"))
	})
	It("rejects a link inside the workspace that points outside", func() {
		v, id, out := decide("/sandbox/work/link/passwd", resolver(map[string]string{"/sandbox/work/link/passwd": "/etc/passwd"}, nil))
		Expect(v).To(Equal(agentd.Reject))
		Expect(id).To(Equal("no"))
		Expect(out).To(Equal("/sandbox/work/link/passwd"))
	})
	It("rejects a path that is itself a link", func() {
		v, _, _ := decide("/sandbox/work/link", resolver(map[string]string{"/sandbox/work/link": "!"}, nil))
		Expect(v).To(Equal(agentd.Reject))
	})
	It("follows a workspace that is itself reached through a link", func() {
		r := func(string, []string) ([]string, error) { return []string{"/data/work", "/data/work/a.txt"}, nil }
		v, _, _ := decide("/sandbox/work/a.txt", r)
		Expect(v).To(Equal(agentd.Allow))
	})
	It("fails closed when the paths cannot be resolved", func() {
		v, _, _ := decide("/sandbox/work/a.txt", resolver(nil, errors.New("sandbox gone")))
		Expect(v).To(Equal(agentd.Reject))
	})
	It("does not resolve a request without paths, or one rejected already", func() {
		calls := 0
		r := func(string, []string) ([]string, error) { calls++; return nil, errors.New("no") }
		v, _, _ := agentd.Decide(agentd.DefaultPermissions(), acp.PermissionRequest{ToolCall: sdk.ToolCallUpdate{Kind: &read}, Options: req("").Options}, "/sandbox/work", r)
		Expect(v).To(Equal(agentd.Allow))
		decide("/etc/passwd", r)
		Expect(calls).To(BeZero())
	})
	It("rejects a path with a line break, which the resolver could not report", func() {
		v, _, _ := decide("/sandbox/work/a\nb", resolver(nil, nil))
		Expect(v).To(Equal(agentd.Reject))
	})

	It("resolves links with the real script", func() {
		root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		ws, outside := filepath.Join(root, "work"), filepath.Join(root, "outside")
		Expect(os.MkdirAll(filepath.Join(ws, "sub"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(outside, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(ws, "sub", "f"), nil, 0o600)).To(Succeed())
		Expect(os.Symlink(outside, filepath.Join(ws, "out"))).To(Succeed())
		Expect(os.Symlink("sub", filepath.Join(ws, "in"))).To(Succeed())
		Expect(os.Symlink(filepath.Join(ws, "sub", "f"), filepath.Join(ws, "flink"))).To(Succeed())
		wslink := filepath.Join(root, "wslink")
		Expect(os.Symlink(ws, wslink)).To(Succeed())
		resolve := func(w string, paths []string) ([]string, error) {
			out, err := exec.Command("sh", append([]string{"-c", agentd.RealpathScript, "sh", w}, paths...)...).Output()
			return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), err
		}
		decide := func(w, p string) agentd.Verdict {
			v, _, _ := agentd.Decide(agentd.DefaultPermissions(), req(p), w, resolve)
			return v
		}
		Expect(decide(ws, filepath.Join(ws, "sub", "f"))).To(Equal(agentd.Allow))
		Expect(decide(ws, filepath.Join(ws, "sub", "new", "file"))).To(Equal(agentd.Allow), "a path that does not exist yet")
		Expect(decide(ws, filepath.Join(ws, "in", "f"))).To(Equal(agentd.Allow), "a link that stays inside")
		Expect(decide(ws, filepath.Join(ws, "out", "x"))).To(Equal(agentd.Reject), "through a link out")
		Expect(decide(ws, filepath.Join(ws, "out", "new", "x"))).To(Equal(agentd.Reject), "a new file below a link out")
		Expect(decide(ws, filepath.Join(ws, "out"))).To(Equal(agentd.Reject), "the link itself")
		Expect(decide(ws, filepath.Join(ws, "flink"))).To(Equal(agentd.Reject), "a leaf link, even one that stays inside")
		Expect(decide(wslink, filepath.Join(wslink, "sub", "f"))).To(Equal(agentd.Allow), "a workspace named through a link")
		Expect(decide(ws, ws)).To(Equal(agentd.Allow), "the workspace")
	})
})
