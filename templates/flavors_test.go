// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/templates"
)

var (
	createFlavorTypeRE = regexp.MustCompile(`(?is)CREATE TYPE agent_flavor AS ENUM \(([^)]*)\)`)
	addFlavorValueRE   = regexp.MustCompile(`(?is)ALTER TYPE agent_flavor ADD VALUE (?:IF NOT EXISTS )?'([^']+)'`)
	sqlLiteralRE       = regexp.MustCompile(`'([^']+)'`)
)

var _ = Describe("flavors", func() {
	want := func() []string {
		var out []string
		for _, f := range templates.AllFlavors {
			out = append(out, string(f))
		}
		return out
	}

	It("match the template schema's flavor enum", func() {
		var schema struct {
			Defs struct {
				Flavor struct {
					Enum []string `json:"enum"`
				} `json:"flavor"`
			} `json:"$defs"`
		}
		Expect(json.Unmarshal(readFile("schema/template.schema.json"), &schema)).To(Succeed())
		Expect(schema.Defs.Flavor.Enum).To(ConsistOf(want()))
	})

	It("match the agent_flavor type in the migrations", func() {
		files, err := filepath.Glob("../internal/db/migrations/*.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())
		var values []string
		created := 0
		for _, f := range files {
			sql := string(readFile(f))
			for _, m := range createFlavorTypeRE.FindAllStringSubmatch(sql, -1) {
				created++
				for _, v := range sqlLiteralRE.FindAllStringSubmatch(m[1], -1) {
					values = append(values, v[1])
				}
			}
			for _, m := range addFlavorValueRE.FindAllStringSubmatch(sql, -1) {
				values = append(values, m[1])
			}
			Expect(strings.Contains(strings.ToUpper(sql), "ALTER TYPE AGENT_FLAVOR RENAME")).To(BeFalse(),
				"%s renames an agent_flavor value; teach this test about it", f)
		}
		Expect(created).To(Equal(1), "CREATE TYPE agent_flavor not found exactly once")
		Expect(values).To(ConsistOf(want()))
	})
})
