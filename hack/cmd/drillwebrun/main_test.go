// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestDrillwebrun(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "drillwebrun")
}

const pinned = "example.invalid/playwright:v1-noble@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

var _ = Describe("dockerArgs", func() {
	base := config{Repo: "/work/koinon", Image: pinned, Spec: "signin.spec", UID: 501, GID: 20}

	It("runs as the host user in the image, in web/, with the drill config", func() {
		args, err := dockerArgs(base)
		Expect(err).NotTo(HaveOccurred())
		Expect(args).To(ContainElements("--user", "501:20", "--network=host", "-w", "/work/koinon/web"))
		Expect(args[len(args)-6:]).To(Equal([]string{pinned, "node_modules/.bin/playwright", "test", "-c", "playwright.drill.config.ts", "signin.spec"}))
	})

	It("mounts the parent of the output directory, never the directory itself (#1072)", func() {
		c := base
		c.OutputDir = "/runner/_temp/drill/browser-signin"
		args, err := dockerArgs(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(args).To(ContainElement("/runner/_temp/drill:/runner/_temp/drill"))
		Expect(args).NotTo(ContainElement("/runner/_temp/drill/browser-signin:/runner/_temp/drill/browser-signin"))
	})

	It("adds no second mount when the repository mount covers the parent", func() {
		c := base
		c.OutputDir = "/work/koinon/out/browser-signin"
		Expect(mountDirs(c)).To(Equal([]string{"/work/koinon"}))
		c.OutputDir = "/work/koinon-other/x"
		Expect(mountDirs(c)).To(HaveLen(2))
	})

	It("passes the variables by name and never by value", func() {
		c := base
		c.Environ = []string{"KOINON_DRILL_WEB_PASSWORD=secret-value", "HTTPS_PROXY=http://p", "HOME=/x", "PATH=/bin", "PLAYWRIGHT_OUTPUT_DIR=/o/x", "KOINON_DRILL_WEB_URL=u"}
		args, err := dockerArgs(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(envNames(c.Environ)).To(Equal([]string{"HTTPS_PROXY", "KOINON_DRILL_WEB_PASSWORD", "KOINON_DRILL_WEB_URL", "PLAYWRIGHT_OUTPUT_DIR"}))
		for _, a := range args {
			Expect(a).NotTo(ContainSubstring("secret-value"))
		}
	})

	It("refuses an image that is not pinned, a relative output dir and a missing spec", func() {
		c := base
		c.Image = "mcr.microsoft.com/playwright:v1-noble"
		_, err := dockerArgs(c)
		Expect(err).To(MatchError(ContainSubstring("pinned by digest")))
		c = base
		c.OutputDir = "rel/out"
		_, err = dockerArgs(c)
		Expect(err).To(MatchError(ContainSubstring("absolute")))
		c = base
		c.Spec = ""
		_, err = dockerArgs(c)
		Expect(err).To(HaveOccurred())
	})
})

// The container image for the integration spec: small, pinned by digest, has sh.
const busybox = "busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

var _ = Describe("the container path", Ordered, func() {
	var repo, out string

	BeforeAll(func() {
		testenv.Require(testenv.Docker)
		// A temp repository: go.mod marks the root, web/node_modules/.bin/playwright is a stand-in that does
		// what Playwright does at start: remove outputDir, then create it and write into it.
		repo = GinkgoT().TempDir()
		for rel, body := range map[string]string{
			"go.mod":                           "module x\n",
			"web/node_modules/.bin/playwright": "#!/bin/sh\nrm -rf \"$PLAYWRIGHT_OUTPUT_DIR\" || exit 1\nmkdir -p \"$PLAYWRIGHT_OUTPUT_DIR\" && echo ok >\"$PLAYWRIGHT_OUTPUT_DIR/result.txt\"\n",
		} {
			p := filepath.Join(repo, rel)
			Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
			Expect(os.WriteFile(p, []byte(body), 0o755)).To(Succeed())
		}
		// The output directory lives outside the repository, like the drill's (RUNNER_TEMP).
		out = filepath.Join(GinkgoT().TempDir(), "drill", "browser-signin")
		Expect(exec.Command("docker", "pull", "-q", busybox).Run()).To(Succeed())
	})

	runTool := func() (int, string) {
		var o, e bytes.Buffer
		env := append(os.Environ(), imageEnv+"="+busybox, outputDirEnv+"="+out)
		code := run([]string{"signin.spec"}, env, repo, "docker", &o, &e)
		return code, o.String() + e.String()
	}

	It("lets Playwright delete and recreate its output directory as a non-root user", func() {
		if os.Getuid() == 0 {
			Skip("running as root: the non-root case is not exercised")
		}
		Expect(os.MkdirAll(out, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(out, "old.txt"), []byte("stale"), 0o644)).To(Succeed())
		code, log := runTool()
		Expect(code).To(BeZero(), log)
		b, err := os.ReadFile(filepath.Join(out, "result.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal("ok\n"))
		Expect(filepath.Join(out, "old.txt")).NotTo(BeAnExistingFile())
	})

	It("creates the output directory when it does not exist yet", func() {
		Expect(os.RemoveAll(out)).To(Succeed())
		code, log := runTool()
		Expect(code).To(BeZero(), log)
		Expect(filepath.Join(out, "result.txt")).To(BeAnExistingFile())
	})

	It("passes the exit code of the spec through", func() {
		Expect(os.WriteFile(filepath.Join(repo, "web/node_modules/.bin/playwright"), []byte("#!/bin/sh\nexit 7\n"), 0o755)).To(Succeed())
		code, _ := runTool()
		Expect(code).To(Equal(7))
	})
})
