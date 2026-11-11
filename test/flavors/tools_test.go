// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// tinyPDF is a one-page PDF whose text is "Hello Koinon" with a correct cross-reference table.
func tinyPDF() string {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 100] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		"", // the content stream, below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	stream := "BT /F1 18 Tf 20 40 Td (Hello Koinon) Tj ET"
	objs[3] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.String()
}

// describeTools registers the data-tools specs for one flavor: every tool runs once inside the
// image as UID 10001 with no network, a read-only root file system and a writable /tmp, the way
// the agent uses them. The image must hold no pip and no way to install anything.
func describeTools(f flavor) bool {
	return Describe(f.name+" data tools", func() {
		BeforeEach(func() {
			if !f.dataTools {
				Skip(f.name + " ships no data tools")
			}
			testenv.Require(testenv.Docker)
			ensureImage(f)
		})

		// sh runs script with /bin/sh in a throwaway container and returns its output and exit
		// code. stdin, when set, is the script's standard input.
		sh := func(ctx context.Context, stdin, script string) (stdout, stderr string, code int) {
			var o, e bytes.Buffer
			c := exec.CommandContext(ctx, "docker", "run", "--rm", "--interactive",
				"--user", sandboxUID, "--network", "none", "--read-only",
				"--tmpfs", "/tmp:rw,exec,mode=1777", "--tmpfs", "/sandbox:rw,mode=0700,uid=10001,gid=10001",
				"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
				"--env", "HOME=/sandbox", "--workdir", "/sandbox",
				"--entrypoint", "/bin/sh", f.image(), "-c", script)
			c.Stdin = strings.NewReader(stdin)
			c.Stdout, c.Stderr = &o, &e
			err := c.Run()
			var exit *exec.ExitError
			switch {
			case err == nil:
			case errors.As(err, &exit):
				code = exit.ExitCode()
			default:
				Expect(err).NotTo(HaveOccurred())
			}
			return o.String(), e.String(), code
		}
		// ok runs script and expects exit 0.
		ok := func(ctx context.Context, stdin, script string) string {
			GinkgoHelper()
			out, errOut, code := sh(ctx, stdin, script)
			Expect(code).To(BeZero(), "%s\nstdout: %s\nstderr: %s", script, out, errOut)
			return out
		}

		DescribeTable("runs the tool once",
			func(ctx SpecContext, stdin, script, want string) {
				Expect(ok(ctx, stdin, script)).To(ContainSubstring(want))
			},
			Entry("jq", SpecTimeout(time.Minute), `{"a":[1,2,3]}`, `jq -c '.a | add'`, "6"),
			Entry("yq reads YAML", SpecTimeout(time.Minute), "a:\n  b: 7\n", `yq '.a.b'`, "7"),
			Entry("yq converts XML to JSON", SpecTimeout(time.Minute), "<r><x>1</x></r>", `yq -p xml -o json -I0 '.'`, `{"r":{"x":"1"}}`),
			Entry("fd", SpecTimeout(time.Minute), "",
				`mkdir /tmp/w && touch /tmp/w/a.txt /tmp/w/b.md && fd -e md . /tmp/w`, "/tmp/w/b.md"),
			Entry("ast-grep", SpecTimeout(time.Minute), "func f() error { return nil }\n",
				`cat > /tmp/x.go && ast-grep run --lang go -p 'func $F() error { $$$ }' --json=compact /tmp/x.go`, `"text":"func f() error { return nil }"`),
			Entry("rg, tree, file, unzip, zstd, xz", SpecTimeout(time.Minute), "",
				`mkdir /tmp/d && echo hello > /tmp/d/a && rg -c hello /tmp/d && tree /tmp/d | tail -1 && file -b /tmp/d/a && zstd -q -c /tmp/d/a | zstd -dc && xz -c /tmp/d/a | xz -dc && unzip -v | head -1`,
				"1 directory, 1 file"),
			Entry("sqlite3", SpecTimeout(time.Minute), "",
				`sqlite3 /tmp/t.db 'create table t(a); insert into t values (7)' && sqlite3 -readonly /tmp/t.db 'select a from t'`, "7"),
			Entry("duckdb select", SpecTimeout(time.Minute), "", `duckdb -noheader -list -c 'select 42'`, "42"),
			Entry("duckdb on a CSV", SpecTimeout(time.Minute), "",
				`printf 'a,b\n1,2\n3,4\n' > /tmp/t.csv && duckdb -json -c 'select sum(b)::int as s from "/tmp/t.csv"'`, `[{"s":6}]`),
			Entry("duckdb on stdin", SpecTimeout(time.Minute), "select 'from stdin' as s;\n", `duckdb -csv`, "from stdin"),
			Entry("duckdb on a database file", SpecTimeout(time.Minute), "",
				`duckdb /tmp/t.duckdb -c 'create table t as select 5 as a' && duckdb -readonly /tmp/t.duckdb -noheader -list -c 'select a from t'`, "5"),
			Entry("duckdb writes and reads Parquet", SpecTimeout(time.Minute), "",
				`duckdb -noheader -list -c "copy (select 9 as n) to '/tmp/t.parquet' (format parquet); select n from '/tmp/t.parquet'"`, "9"),
			Entry("python3 and python", SpecTimeout(time.Minute), "",
				`python3 -c 'import json, csv, sqlite3, gzip, zipfile, statistics, decimal, datetime, re, subprocess; print(statistics.mean([1, 2, 3]))' && python -c 'print("py")'`, "py"),
		)

		It("reads a PDF", func(ctx SpecContext) {
			out := ok(ctx, tinyPDF(), `cat > /tmp/t.pdf && pdfinfo /tmp/t.pdf && pdftotext -layout -f 1 -l 1 /tmp/t.pdf -`)
			Expect(out).To(ContainSubstring("Pages:           1"))
			Expect(out).To(ContainSubstring("Hello Koinon"))
		}, SpecTimeout(time.Minute))

		It("applies the DuckDB defaults, which a query may still change", func(ctx SpecContext) {
			out := ok(ctx, "", `duckdb -noheader -csv -c "select current_setting('memory_limit'), current_setting('threads'), `+
				`current_setting('autoinstall_known_extensions'), current_setting('autoload_known_extensions'), current_setting('temp_directory')"`)
			Expect(strings.TrimSpace(out)).To(Equal("768.0 MiB,2,false,false,/tmp/duckdb"))
			Expect(ok(ctx, "", `duckdb -noheader -list -c "set memory_limit='512MiB'; select current_setting('memory_limit')"`)).
				To(Equal("512.0 MiB\n"))
			// Extension autoload is really off: a remote CSV needs httpfs, which must not be fetched.
			_, stderr, code := sh(ctx, "", `duckdb -c "select * from read_csv('https://example.com/x.csv')"`)
			Expect(code).NotTo(BeZero())
			Expect(stderr).To(ContainSubstring("Missing Extension"))
			// A failed query exits non-zero and says why on stderr, nothing else on stdout.
			stdout, stderr, code := sh(ctx, "", `duckdb -c 'select * from missing_table'`)
			Expect(code).NotTo(BeZero())
			Expect(stdout).To(BeEmpty())
			Expect(stderr).To(ContainSubstring("missing_table"))
		}, SpecTimeout(time.Minute))

		It("cannot install anything: no pip, no package manager that works, no network, no formatter", func(ctx SpecContext) {
			for _, tool := range []string{"pip", "pip3", "ensurepip", "uv", "ruff", "pipx", "virtualenv"} {
				_, _, code := sh(ctx, "", "command -v "+tool)
				Expect(code).NotTo(BeZero(), "%s is in the image", tool)
			}
			for _, script := range []string{
				`python3 -m pip --version`, `python3 -m ensurepip`, `python3 -m venv /tmp/v`,
				`python3 -c 'import socket; socket.create_connection(("1.1.1.1", 443), 3)'`,
				`duckdb -c 'install httpfs'`,
			} {
				_, _, code := sh(ctx, "", script)
				Expect(code).NotTo(BeZero(), script)
			}
			// The entry script keeps agent-written Python from leaving __pycache__ in a repository.
			Expect(ok(ctx, "", `grep -c '^export PYTHONDONTWRITEBYTECODE=1$' /usr/local/bin/koinon-`+f.name)).To(Equal("1\n"))
		}, SpecTimeout(3*time.Minute))
	})
}

// sizeBudgetLine is the one line the size guard reports for a measured image, passing or not: the
// measured KiB, the budget and the headroom (negative when over). A bump that grows the image reads
// the headroom here and raises sizeBudget on purpose, instead of finding out from a red test.
func sizeBudgetLine(image, arch string, kib, budget int64) string {
	return fmt.Sprintf("%s (%s): %d KiB measured, budget %d KiB, headroom %d KiB (%.1f%%)",
		image, arch, kib, budget, budget-kib, 100*float64(budget-kib)/float64(budget))
}

// sizeBudgetFailure is the failure message when the image is over budget: where the budget lives
// and how it was measured, so raising it is a deliberate edit with the new number in hand.
func sizeBudgetFailure(f flavor, arch string, kib, budget int64) string {
	return fmt.Sprintf("%s\nover budget by %d KiB. To raise it on purpose, edit sizeBudget[%q] of the %q flavor in test/flavors/%s_test.go "+
		"to the measured %d KiB plus about 10%%, update the measurement comment above it and the size note in flavors/%s/README.md",
		sizeBudgetLine(f.image(), arch, kib, budget), kib-budget, arch, f.name, f.name, kib, f.name)
}

var _ = Describe("size budget report", func() {
	It("reports measured, budget and headroom, and says where to raise the budget", func() {
		Expect(sizeBudgetLine("img", "arm64", 650_000, 716_000)).To(Equal("img (arm64): 650000 KiB measured, budget 716000 KiB, headroom 66000 KiB (9.2%)"))
		Expect(sizeBudgetLine("img", "amd64", 700_000, 696_000)).To(ContainSubstring("headroom -4000 KiB"))
		msg := sizeBudgetFailure(flavor{name: "opencode"}, "amd64", 700_000, 696_000)
		Expect(msg).To(ContainSubstring("over budget by 4000 KiB"))
		Expect(msg).To(ContainSubstring(`sizeBudget["amd64"]`))
		Expect(msg).To(ContainSubstring("test/flavors/opencode_test.go"))
	})
})

// describeImageSize registers the image-size guard for one flavor.
func describeImageSize(f flavor) bool {
	return Describe(f.name+" image size", func() {
		It("stays within its budget", func(ctx SpecContext) {
			if len(f.sizeBudget) == 0 {
				Skip(f.name + " has no size budget")
			}
			testenv.Require(testenv.Docker)
			ensureImage(f)
			arch := strings.TrimSpace(docker(ctx, "image", "inspect", "--format", "{{.Architecture}}", f.image()))
			budget, ok := f.sizeBudget[arch]
			Expect(ok).To(BeTrue(), "no size budget for %s in flavor %s", arch, f.name)
			var kib int64
			out := docker(ctx, "run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "du", f.image(), "-sxk", "/")
			_, err := fmt.Sscanf(out, "%d", &kib)
			Expect(err).NotTo(HaveOccurred(), out)
			// Reported whether it passes or not (the spec's report entries and the verbose output).
			line := sizeBudgetLine(f.image(), arch, kib, budget)
			AddReportEntry("image size", line)
			GinkgoWriter.Println(line)
			Expect(kib).To(BeNumerically("<=", budget), sizeBudgetFailure(f, arch, kib, budget))
		}, SpecTimeout(time.Minute))
	})
}

var _ = func() bool {
	for _, f := range flavors {
		describeTools(f)
		describeImageSize(f)
	}
	return true
}()
