// SPDX-License-Identifier: Apache-2.0

// Package rt1 checks the RT1 fixture configuration that parallel runs depend on (issue 62): the
// SearXNG settings, the prometheus-mcp pod and the motherduck init SQL.
package rt1_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

func TestRT1Fixtures(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RT1 fixtures")
}

func readFile(parts ...string) string {
	b, err := os.ReadFile(filepath.Join(parts...))
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

func docs(path string) []map[string]any {
	var out []map[string]any
	for _, part := range strings.Split(readFile(path), "\n---\n") {
		var m map[string]any
		Expect(yaml.Unmarshal([]byte(part), &m)).To(Succeed())
		if len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

var _ = Describe("searxng-settings.yml", func() {
	var settings map[string]any
	BeforeEach(func() {
		Expect(yaml.Unmarshal([]byte(readFile("manifests", "searxng-settings.yml")), &settings)).To(Succeed())
	})

	It("suspends a rate-limited engine for 60 seconds, not the hour default", func() {
		Expect(dig(settings, "search", "suspended_times", "SearxEngineTooManyRequests")).To(BeEquivalentTo(60))
	})

	It("runs mojeek next to brave", func() {
		Expect(dig(settings, "use_default_settings", "engines", "keep_only")).To(ContainElements("brave", "mojeek"))
		engines, _ := settings["engines"].([]any)
		Expect(engines).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "mojeek"), HaveKeyWithValue("inactive", false))))
	})
})

var _ = Describe("prometheus.yaml", func() {
	var objs []map[string]any
	BeforeEach(func() { objs = docs("manifests/prometheus.yaml") })

	find := func(kind string) map[string]any {
		for _, o := range objs {
			if o["kind"] == kind {
				return o
			}
		}
		Fail("no " + kind)
		return nil
	}

	It("bounds prometheus-mcp's upstream calls and CPU", func() {
		spec := dig(find("Deployment"), "spec", "template", "spec")
		var mcp map[string]any
		for _, c := range dig(spec, "containers").([]any) {
			if dig(c, "name") == "prometheus-mcp" {
				mcp = c.(map[string]any)
			}
		}
		Expect(mcp).NotTo(BeNil())
		Expect(mcp["env"]).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "PROMETHEUS_REQUEST_TIMEOUT"), HaveKeyWithValue("value", "10"))))
		Expect(dig(mcp, "resources", "limits", "cpu")).To(Equal("1"))
	})

	It("runs in a PriorityClass the manifest defines", func() {
		name := dig(find("Deployment"), "spec", "template", "spec", "priorityClassName")
		Expect(name).To(Equal(dig(find("PriorityClass"), "metadata", "name")))
	})
})

var _ = Describe("motherduck", func() {
	It("does not lock the configuration, which breaks every concurrent connection", func() {
		Expect(readFile("motherduck", "init_sql.py")).To(ContainSubstring("memory_limit"))
		Expect(readFile("motherduck", "startup.py")).NotTo(ContainSubstring("SET lock_configuration"))
	})

	It("serves 16 concurrent connections (python unittest; the image build always runs it, this needs python3 and duckdb)", func() {
		py, err := exec.LookPath("python3")
		if err != nil {
			Skip("python3 not found")
		}
		if exec.Command(py, "-c", "import duckdb").Run() != nil {
			Skip("python3 has no duckdb module")
		}
		cmd := exec.Command(py, "-B", "-m", "unittest", "test_init_sql")
		cmd.Dir = "motherduck"
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
	})
})
