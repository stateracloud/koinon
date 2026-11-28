// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

// These tests run the installer's own export step (exportCommit, the body of exportPin) with the real git
// against a source that has no ref at all: the cache of `kind/substrate.sh` fetches the pinned commit by its
// id, so the commit sits there only as FETCH_HEAD. A bare clone of such a source is empty (issue #1091).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/substrateinstall"
	"github.com/stateracloud/koinon/internal/testing/gitenv"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// gitIn runs git in dir with a fixed identity and no global config, and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := gitenv.Command(t.Context(), t.TempDir(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// refLessCache makes a cache like the one of kind/substrate.sh: an empty repository that holds one commit
// only as FETCH_HEAD, and returns the cache and the commit id.
func refLessCache(t *testing.T) (cache, commit string) {
	t.Helper()
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "pinned.txt"), []byte("pinned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "add", ".")
	gitIn(t, origin, "commit", "-q", "-m", "pinned")
	commit = gitIn(t, origin, "rev-parse", "HEAD")

	cache = t.TempDir()
	gitIn(t, cache, "init", "-q")
	gitIn(t, cache, "fetch", "-q", "--depth", "1", origin, commit)
	if refs := gitIn(t, cache, "for-each-ref"); refs != "" {
		t.Fatalf("the cache has refs: %s", refs)
	}
	return cache, commit
}

func TestExportsACommitFromACacheWithoutRefs(t *testing.T) {
	testenv.RequireT(t, gitBinary)
	g := NewWithT(t)
	cache, commit := refLessCache(t)
	inst := &substrateinstall.Installer{Runner: execRunner{}, SourceRepo: cache, Out: &strings.Builder{}}
	dst := filepath.Join(t.TempDir(), "src")

	g.Expect(inst.ExportCommit(context.Background(), t.TempDir(), dst, commit)).To(Succeed())

	got, err := os.ReadFile(filepath.Join(dst, "pinned.txt"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal("pinned\n"))
}

func TestRefusesACommitThatTheSourceDoesNotHold(t *testing.T) {
	testenv.RequireT(t, gitBinary)
	g := NewWithT(t)
	cache, _ := refLessCache(t)
	inst := &substrateinstall.Installer{Runner: execRunner{}, SourceRepo: cache, Out: &strings.Builder{}}

	err := inst.ExportCommit(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "src"), strings.Repeat("a", 40))

	g.Expect(err).To(MatchError(substrateinstall.ErrPin))
	g.Expect(err).To(MatchError(ContainSubstring("is not in " + cache)))
}

func TestExportsACommitFromARelativeLocalSource(t *testing.T) {
	testenv.RequireT(t, gitBinary)
	g := NewWithT(t)
	cache, commit := refLessCache(t)
	// The caller runs in the parent directory of the source and names it with a relative path, as in
	// --substrate-source ./substrate. The fetch runs under git -C <temporary repository>, so the path must be made
	// absolute against this directory first (issue #1530).
	t.Chdir(filepath.Dir(cache))
	inst := &substrateinstall.Installer{Runner: execRunner{}, SourceRepo: "./" + filepath.Base(cache), Out: &strings.Builder{}}
	dst := filepath.Join(t.TempDir(), "src")

	g.Expect(inst.ExportCommit(context.Background(), t.TempDir(), dst, commit)).To(Succeed())

	got, err := os.ReadFile(filepath.Join(dst, "pinned.txt"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal("pinned\n"))
}
