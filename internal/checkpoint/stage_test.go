// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func makeTar(entries ...entry) *bytes.Buffer {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		Expect(tw.WriteHeader(h)).To(Succeed())
		if typ == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			Expect(err).NotTo(HaveOccurred())
		}
	}
	Expect(tw.Close()).To(Succeed())
	return &b
}

var _ = Describe("Stage", func() {
	var dir string
	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("extracts files, directories and symbolic links", func() {
		st, err := checkpoint.Stage(makeTar(
			entry{name: "./", typ: tar.TypeDir, mode: 0o755},
			entry{name: "./notes/", typ: tar.TypeDir, mode: 0o755},
			entry{name: "./notes/a.md", body: "hello"},
			entry{name: "./link", typ: tar.TypeSymlink, link: "notes/a.md"},
		), dir, checkpoint.DefaultLimits)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Files).To(BeEquivalentTo(1))
		b, err := os.ReadFile(filepath.Join(dir, "notes", "a.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal("hello"))
		l, err := os.Readlink(filepath.Join(dir, "link"))
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(Equal("notes/a.md"))
	})

	It("refuses names outside the home", func() {
		for _, n := range []string{"/etc/passwd", "../x", "a/../../x"} {
			_, err := checkpoint.Stage(makeTar(entry{name: n, body: "x"}), GinkgoT().TempDir(), checkpoint.DefaultLimits)
			Expect(err).To(HaveOccurred(), n)
		}
	})

	It("does not write through a symbolic link that leaves the directory", func() {
		outside := GinkgoT().TempDir()
		_, err := checkpoint.Stage(makeTar(
			entry{name: "esc", typ: tar.TypeSymlink, link: outside},
			entry{name: "esc/pwned", body: "x"},
		), dir, checkpoint.DefaultLimits)
		Expect(err).To(HaveOccurred())
		Expect(filepath.Join(outside, "pwned")).NotTo(BeAnExistingFile())
	})

	It("copies a hard link to a staged file and skips links to anything else", func() {
		st, err := checkpoint.Stage(makeTar(
			entry{name: "a", body: "shared"},
			entry{name: "sub/b", typ: tar.TypeLink, link: "a"},
			entry{name: "gone", typ: tar.TypeLink, link: "nothing"},
		), dir, checkpoint.DefaultLimits)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Files).To(BeEquivalentTo(2))
		Expect(st.Skipped).To(BeEquivalentTo(1))
		b, err := os.ReadFile(filepath.Join(dir, "sub", "b"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal("shared"))
	})

	It("skips hard links outside the home, devices and excluded paths", func() {
		st, err := checkpoint.Stage(makeTar(
			entry{name: "h", typ: tar.TypeLink, link: "/etc/passwd"},
			entry{name: "dev", typ: tar.TypeChar},
			entry{name: "proj/node_modules/x/index.js", body: "x"},
			entry{name: ".cache/y", body: "x"},
			entry{name: "keep", body: "x"},
		), dir, checkpoint.DefaultLimits)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Skipped).To(BeEquivalentTo(4))
		Expect(filepath.Join(dir, "keep")).To(BeAnExistingFile())
		Expect(filepath.Join(dir, "proj", "node_modules")).NotTo(BeADirectory())
	})

	It("enforces the size and entry limits", func() {
		_, err := checkpoint.Stage(makeTar(entry{name: "big", body: "0123456789"}), dir, checkpoint.Limits{MaxBytes: 5})
		Expect(err).To(MatchError(checkpoint.ErrTooLarge))
		_, err = checkpoint.Stage(makeTar(entry{name: "a", body: "1"}, entry{name: "b", body: "1"}), GinkgoT().TempDir(), checkpoint.Limits{MaxEntries: 1})
		Expect(err).To(MatchError(checkpoint.ErrTooLarge))
	})

	It("keeps a read-only directory's contents and its mode", func() {
		_, err := checkpoint.Stage(makeTar(
			entry{name: "ro/", typ: tar.TypeDir, mode: 0o555},
			entry{name: "ro/f", body: "x", mode: 0o444},
		), dir, checkpoint.DefaultLimits)
		Expect(err).NotTo(HaveOccurred())
		fi, err := os.Stat(filepath.Join(dir, "ro"))
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o555)))
		DeferCleanup(checkpoint.RemoveAll, dir)
	})
})

var _ = Describe("Excluded", func() {
	DescribeTable("paths",
		func(p string, want bool) { Expect(checkpoint.Excluded(p)).To(Equal(want)) },
		Entry("a cache at the top", ".cache/pip/x", true),
		Entry("node_modules anywhere", "work/app/node_modules/a", true),
		Entry("a session", ".local/share/opencode/sessions/s1.json", false),
		Entry("notes", "notes/today.md", false),
		Entry("a similar name", "my.cache/x", false),
		Entry("with ./", "./.cargo/registry/x", true),
		Entry("lost+found at the top, with ./", "./lost+found", true),
		Entry("lost+found at the top", "lost+found", true),
		Entry("inside lost+found", "lost+found/x", true),
		Entry("lost+found deeper is a user's directory", "work/lost+found/x", false),
	)
})

var _ = Describe("TarExcludeArgs", func() {
	It("leaves out lost+found at the top of the home only", func() {
		Expect(checkpoint.TarExcludeArgs()).To(ContainElement("--exclude=./lost+found"))
		Expect(checkpoint.TarExcludeArgs()).NotTo(ContainElement("--exclude=lost+found"))
	})
})

var _ = Describe("WriteTar", func() {
	It("writes a tar for the sandbox's fixed `tar xf - -C /sandbox`: no root entry, no excluded paths, no owners", func() {
		dir := GinkgoT().TempDir()
		put := func(name, body string) {
			Expect(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)).To(Succeed())
		}
		put("notes/today.md", "remember the milk")
		put("lost+found/orphan", "fsck")
		put("work/lost+found/x", "a user's")
		put("work/app/node_modules/x/index.js", "cache")
		put(".cache/pip/x", "cache")
		put("tmp/scratch", "scratch")
		Expect(os.Symlink("notes/today.md", filepath.Join(dir, "link"))).To(Succeed())

		var buf bytes.Buffer
		Expect(checkpoint.WriteTar(dir, &buf)).To(Succeed())
		var names []string
		tr := tar.NewReader(&buf)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			Expect(err).NotTo(HaveOccurred())
			names = append(names, h.Name)
			Expect(h.Uid).To(BeZero())
			Expect(h.Gid).To(BeZero())
			Expect(h.Uname).To(BeEmpty())
			Expect(h.Gname).To(BeEmpty())
		}
		Expect(names).To(ConsistOf("notes/", "notes/today.md", "work/", "work/lost+found/", "work/lost+found/x", "work/app/", "link"))
		for _, n := range names {
			Expect(n).NotTo(Or(Equal("."), Equal("./"), HavePrefix("./"), HavePrefix("/")))
		}
	})
})
