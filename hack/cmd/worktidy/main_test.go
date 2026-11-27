// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/gitenv"
)

func TestWorktidy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "worktidy")
}

type fakeGit struct {
	wts       []Worktree
	dirty     map[string]int
	unpushed  map[string]int
	branches  []Branch
	removed   []string
	forced    []string
	deleted   []string
	patched   []string
	patchErr  error
	removeErr map[string]error
}

func (f *fakeGit) Worktrees() ([]Worktree, error) {
	return append([]Worktree(nil), f.wts...), nil
}

func (f *fakeGit) Status(p string) (int, error) {
	return f.dirty[p], nil
}

func (f *fakeGit) Unpushed(p string) (int, error) {
	return f.unpushed[p], nil
}

func (f *fakeGit) Branches() ([]Branch, error) {
	return f.branches, nil
}

func (f *fakeGit) DeleteBranch(n string) error {
	f.deleted = append(f.deleted, n)
	return nil
}

func (f *fakeGit) RemoveForce(w Worktree) error {
	f.forced = append(f.forced, w.Path)
	return nil
}

func (f *fakeGit) RemoveClean(w Worktree) error {
	if err := f.removeErr[w.Path]; err != nil {
		return err
	}
	f.removed = append(f.removed, w.Path)
	return nil
}

func (f *fakeGit) SavePatch(w Worktree, dir string) (string, error) {
	if f.patchErr != nil {
		return "", f.patchErr
	}
	f.patched = append(f.patched, w.Path)
	return dir + "/" + w.Name(), nil
}

type fakeForge struct {
	prs map[string][]PR
	err error
}

func (f fakeForge) PRs() (map[string][]PR, error) {
	return f.prs, f.err
}

type fakeClaims struct {
	cs      []Claim
	err     error
	other   map[int]string
	heldErr error
}

func (f fakeClaims) Mine() ([]Claim, error) {
	return f.cs, f.err
}

// Held answers from other: issue -> holder of a claim of another identity.
func (f fakeClaims) Held(issue int) (string, bool, error) {
	if f.heldErr != nil {
		return "", false, f.heldErr
	}
	h, ok := f.other[issue]
	return h, ok, nil
}

type fakeDocker struct {
	facts      DockerFacts
	containers int
	images     int
}

func (f *fakeDocker) Facts() DockerFacts {
	return f.facts
}

func (f *fakeDocker) PruneContainers() error {
	f.containers++
	return nil
}

func (f *fakeDocker) PruneImages() error {
	f.images++
	return nil
}

const (
	root  = "/tmp/kw"
	mainP = "/repo"
	tip   = "aaaa"
)

func wt(name, branch string) Worktree {
	return Worktree{Path: root + "/" + name, Head: tip, Branch: branch}
}

func merged(branch string) map[string][]PR {
	return map[string][]PR{branch: {{Number: 7, State: "MERGED", Branch: branch, HeadOID: tip}}}
}

var _ = Describe("decideWorktree", func() {
	base := func() Worktree { w := wt("a", "feat/1-a"); w.Managed = true; return w }

	It("removes a merged, clean worktree", func() {
		d := decideWorktree(base(), merged("feat/1-a")["feat/1-a"], true, false, false)
		Expect(d.Action).To(Equal(ActRemove))
	})
	It("keeps a merged, dirty worktree", func() {
		w := base()
		w.Dirty = 46
		d := decideWorktree(w, merged("feat/1-a")["feat/1-a"], true, false, false)
		Expect(d.Action).To(Equal(ActKeep))
		Expect(d.Reason).To(ContainSubstring("46 uncommitted"))
	})
	It("patches then removes a merged, dirty worktree with the flag", func() {
		w := base()
		w.Dirty = 3
		d := decideWorktree(w, merged("feat/1-a")["feat/1-a"], true, false, true)
		Expect(d.Action).To(Equal(ActPatchRemove))
	})
	It("keeps a worktree with an open pull request, even a dirty one with the flag", func() {
		w := base()
		w.Dirty = 1
		prs := []PR{{Number: 9, State: "OPEN", HeadOID: tip}, {Number: 7, State: "MERGED", HeadOID: tip}}
		Expect(decideWorktree(w, prs, true, false, true).Action).To(Equal(ActKeep))
	})
	It("keeps a worktree with no pull request and unpushed commits", func() {
		w := base()
		w.Unpushed = 2
		d := decideWorktree(w, nil, true, false, true)
		Expect(d.Action).To(Equal(ActKeep))
		Expect(d.Reason).To(ContainSubstring("2 commit(s) on no remote branch"))
	})
	It("keeps a merged worktree that has commits the pull request did not carry", func() {
		w := base()
		w.Unpushed = 1
		prs := []PR{{Number: 7, State: "MERGED", HeadOID: "other"}}
		Expect(decideWorktree(w, prs, true, false, false).Action).To(Equal(ActKeep))
	})
	It("reports a broken worktree pointer", func() {
		w := base()
		w.Broken = true
		Expect(decideWorktree(w, nil, true, false, true).Action).To(Equal(ActReport))
	})
	It("keeps the main checkout, the current worktree, an unmanaged one and a locked one", func() {
		for _, mod := range []func(*Worktree){
			func(w *Worktree) { w.Main = true },
			func(w *Worktree) { w.Current = true },
			func(w *Worktree) { w.Managed = false },
			func(w *Worktree) { w.Locked = true },
		} {
			w := base()
			mod(&w)
			Expect(decideWorktree(w, merged("feat/1-a")["feat/1-a"], true, false, true).Action).To(Equal(ActKeep))
		}
	})
	It("removes nothing when the pull request state is unknown", func() {
		d := decideWorktree(base(), nil, false, false, true)
		Expect(d.Action).To(Equal(ActKeep))
		Expect(d.Reason).To(HavePrefix("unknown"))
	})
	It("keeps a merged worktree while a claim is held", func() {
		Expect(decideWorktree(base(), merged("feat/1-a")["feat/1-a"], true, true, false).Action).To(Equal(ActKeep))
	})
	It("keeps a detached worktree", func() {
		w := base()
		w.Branch = ""
		Expect(decideWorktree(w, nil, true, false, true).Action).To(Equal(ActKeep))
	})
})

var _ = Describe("decideBranch", func() {
	prot := map[string]bool{"main": true}
	b := Branch{Name: "feat/1-a", Tip: tip}

	It("removes a branch whose pull request merged with this tip", func() {
		Expect(decideBranch(b, nil, merged("feat/1-a")["feat/1-a"], true, prot).Action).To(Equal(ActRemove))
	})
	It("keeps a branch checked out in a worktree", func() {
		d := decideBranch(b, map[string]string{"feat/1-a": "/tmp/kw/x"}, merged("feat/1-a")["feat/1-a"], true, prot)
		Expect(d.Action).To(Equal(ActKeep))
		Expect(d.Reason).To(ContainSubstring("/tmp/kw/x"))
	})
	It("keeps a branch with no pull request, as a fresh branch is an ancestor of main too", func() {
		Expect(decideBranch(b, nil, nil, true, prot).Action).To(Equal(ActKeep))
	})
	It("keeps a branch that has other commits than the merged pull request", func() {
		prs := []PR{{Number: 7, State: "MERGED", HeadOID: "zzzz"}}
		Expect(decideBranch(b, nil, prs, true, prot).Action).To(Equal(ActKeep))
	})
	It("keeps a branch when gh failed, and a protected branch", func() {
		Expect(decideBranch(b, nil, nil, false, prot).Action).To(Equal(ActKeep))
		Expect(decideBranch(Branch{Name: "main", Tip: tip}, nil, merged("main")["main"], true, prot).Action).To(Equal(ActKeep))
	})
})

var _ = Describe("decideClaim", func() {
	It("reports a claim with no worktree and no open pull request", func() {
		d := decideClaim(Claim{Issue: 5}, map[int]bool{}, map[int]bool{}, map[int]bool{}, true)
		Expect(d.Action).To(Equal(ActReport))
		Expect(d.Reason).To(ContainSubstring("hack/claim.sh release 5"))
	})
	It("keeps a claim that has a worktree, an open pull request, or an unknown state", func() {
		Expect(decideClaim(Claim{Issue: 5}, map[int]bool{5: true}, nil, nil, true).Action).To(Equal(ActKeep))
		Expect(decideClaim(Claim{Issue: 5}, nil, map[int]bool{5: true}, nil, true).Action).To(Equal(ActKeep))
		Expect(decideClaim(Claim{Issue: 5, PR: 8}, nil, nil, map[int]bool{8: true}, true).Action).To(Equal(ActKeep))
		Expect(decideClaim(Claim{Issue: 5}, nil, nil, nil, false).Action).To(Equal(ActKeep))
	})
})

var _ = Describe("patch and docker helpers", func() {
	It("puts the default patch directory in the git common directory, absolute", func() {
		Expect(defaultPatchDir("/repo", ".git\n")).To(Equal("/repo/.git/worktidy-patches"))
		Expect(defaultPatchDir("/repo/wt", "/repo/.git\n")).To(Equal("/repo/.git/worktidy-patches"))
	})
	It("names a patch after the worktree, the branch and a UTC time", func() {
		w := Worktree{Path: "/tmp/kw/dirty", Branch: "feat/2-dirty"}
		n := patchName(w, time.Date(2026, 11, 24, 1, 2, 3, 0, time.UTC))
		Expect(n).To(Equal("dirty-feat-2-dirty-20261124T010203Z"))
	})
	It("never reuses a patch directory", func() {
		repo := GinkgoT().TempDir()
		// execGit starts git with the process environment: isolate it for this spec.
		for _, kv := range gitenv.Env(repo) {
			k, v, _ := strings.Cut(kv, "=")
			GinkgoT().Setenv(k, v)
		}
		for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "x"}} {
			Expect(gitenv.Command(context.Background(), repo, repo, args...).Run()).To(Succeed())
		}
		Expect(os.WriteFile(filepath.Join(repo, "new.txt"), []byte("hi\n"), 0o600)).To(Succeed())
		dir := filepath.Join(GinkgoT().TempDir(), "patches")
		g := &execGit{dir: repo, toplevel: repo}
		w := Worktree{Path: repo, Branch: "feat/1-x"}
		p1, err := g.SavePatch(w, dir)
		Expect(err).NotTo(HaveOccurred())
		p2, err := g.SavePatch(w, dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(p2).NotTo(Equal(p1))
		Expect(filepath.Join(p1, "untracked.patch")).To(BeARegularFile())
	})
	It("reads the reclaimable size of the Images row of docker system df", func() {
		df := "Images\t1.5GB (45%)\nContainers\t0B (0%)\nLocal Volumes\t2GB (10%)\n"
		Expect(parseDfReclaimable(df)).To(Equal("1.5GB"))
		Expect(parseDfReclaimable("Containers\t0B (0%)\n")).To(Equal(""))
	})
	It("labels the unused image count as an estimate", func() {
		ds := decideDocker(DockerFacts{Stopped: 1, UnusedImgs: 2, UnusedSize: "1.5GB"}, false)
		Expect(ds[1].Reason).To(And(ContainSubstring("about 2"), ContainSubstring("estimate"), ContainSubstring("1.5GB")))
	})
	It("reads claim.sh show", func() {
		h, ok := parseShow("#5: held\n  holder: bob#lane\n  lane:   lane\n")
		Expect(ok).To(BeTrue())
		Expect(h).To(Equal("bob#lane"))
		_, ok = parseShow("#5: no claim\n")
		Expect(ok).To(BeFalse())
		_, ok = parseShow("#5: released\n  holder: bob#lane\n")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("decideDocker", func() {
	It("refuses while a container runs or a kind cluster exists, unless forced", func() {
		f := DockerFacts{Running: []string{"web"}, Stopped: 2, UnusedImgs: 3}
		Expect(decideDocker(f, false)).To(HaveLen(1))
		Expect(decideDocker(f, false)[0].Action).To(Equal(ActKeep))
		Expect(decideDocker(DockerFacts{Kind: []string{"kind-control-plane"}}, false)[0].Reason).To(ContainSubstring("kind"))
		forced := decideDocker(f, true)
		Expect(forced[0].Action).To(Equal(ActRemove))
		Expect(forced[1].Action).To(Equal(ActRemove))
	})
	It("reports unknown when docker is unreachable", func() {
		d := decideDocker(DockerFacts{Err: "no socket"}, true)
		Expect(d[0].Action).To(Equal(ActKeep))
	})
})

var _ = Describe("parsers and scope", func() {
	It("reads git worktree list --porcelain, main first", func() {
		wts := parseWorktrees("worktree /repo\nHEAD abc\nbranch refs/heads/main\n\nworktree /tmp/kw/x\nHEAD def\ndetached\nlocked\n\n")
		Expect(wts).To(HaveLen(2))
		Expect(wts[0].Main).To(BeTrue())
		Expect(wts[1].Locked).To(BeTrue())
		Expect(wts[1].Branch).To(BeEmpty())
	})
	It("reads claim.sh mine", func() {
		cs := parseClaims("#12\tlane-a\tpr 40\tme#lane-a\n#13\tlane-b\tpr \tme#lane-b\n")
		Expect(cs).To(Equal([]Claim{{Issue: 12, Lane: "lane-a", PR: 40}, {Issue: 13, Lane: "lane-b"}}))
	})
	It("agrees with wt.sh on the worktree root", func() {
		Expect(isManaged("/tmp/kw/a", "/tmp/kw", false, nil)).To(BeTrue())
		Expect(isManaged("/private/tmp/claude-502/kw/a", "/tmp/kw", false, nil)).To(BeTrue())
		Expect(isManaged("/Users/me/other/a", "/tmp/kw", false, nil)).To(BeFalse())
		Expect(isManaged("/some/other/kw/x", "/tmp/kw", false, nil)).To(BeFalse())
		Expect(isManaged("/opt/tmpbase/kw/x", "/tmp/kw", false, []string{"/opt/tmpbase"})).To(BeTrue())
	})
	It("manages only the root when KOINON_WT_ROOT is set", func() {
		Expect(isManaged("/some/other/kw/x", "/data/wt", true, nil)).To(BeFalse())
		Expect(isManaged("/tmp/kw/x", "/data/wt", true, nil)).To(BeFalse())
		Expect(isManaged("/data/wt/x", "/data/wt", true, nil)).To(BeTrue())
	})
	It("finds the issue of a branch", func() {
		Expect(issueOfBranch("chore/1231-tidy-tool")).To(Equal(1231))
		Expect(issueOfBranch("main")).To(Equal(0))
	})
})

var _ = Describe("run", func() {
	var (
		g   *fakeGit
		dk  *fakeDocker
		dep Deps
		out *bytes.Buffer
	)
	BeforeEach(func() {
		g = &fakeGit{
			wts: []Worktree{
				{Path: mainP, Head: "m", Branch: "main", Main: true},
				wt("done", "feat/1-done"),
				wt("dirty", "feat/2-dirty"),
				wt("open", "feat/3-open"),
				wt("broken", "feat/4-broken"),
			},
			dirty:    map[string]int{root + "/dirty": 4},
			unpushed: map[string]int{},
			branches: []Branch{
				{Name: "main", Tip: "m"},
				{Name: "feat/1-done", Tip: tip},
				{Name: "feat/2-dirty", Tip: tip},
				{Name: "feat/9-gone", Tip: tip},
				{Name: "feat/3-open", Tip: tip},
			},
		}
		g.wts[4].Broken = true
		prs := map[string][]PR{}
		for _, b := range []string{"feat/1-done", "feat/2-dirty", "feat/9-gone"} {
			prs[b] = merged(b)[b]
		}
		prs["feat/3-open"] = []PR{{Number: 3, State: "OPEN", HeadOID: tip}}
		dk = &fakeDocker{facts: DockerFacts{Stopped: 1, UnusedImgs: 2}}
		dep = Deps{Git: g, Forge: fakeForge{prs: prs}, Claims: fakeClaims{cs: []Claim{{Issue: 77, Lane: "x"}}}, Docker: dk, Cwd: mainP, WTRoot: root}
		out = &bytes.Buffer{}
	})

	It("keeps a worktree whose issue is claimed by another identity", func() {
		dep.Claims = fakeClaims{other: map[int]string{1: "bob#lane"}}
		decs, err := plan(dep, Options{})
		Expect(err).NotTo(HaveOccurred())
		for _, dc := range decs {
			if dc.Kind == "worktree" && dc.Name == "done" {
				Expect(dc.Action).To(Equal(ActKeep))
				Expect(dc.Reason).To(ContainSubstring("held by bob#lane"))
			}
		}
	})

	It("keeps a worktree when the claim state of its issue is unknown", func() {
		dep.Claims = fakeClaims{heldErr: errors.New("gh failed")}
		decs, _ := plan(dep, Options{})
		for _, dc := range decs {
			if dc.Kind == "worktree" && dc.Name == "done" {
				Expect(dc.Action).To(Equal(ActKeep))
				Expect(dc.Reason).To(HavePrefix("unknown: claim state"))
			}
		}
	})

	It("changes nothing in a dry run", func() {
		Expect(run(dep, Options{WithDocker: true}, out, &bytes.Buffer{})).To(Equal(0))
		Expect(g.removed).To(BeEmpty())
		Expect(g.forced).To(BeEmpty())
		Expect(g.deleted).To(BeEmpty())
		Expect(dk.containers + dk.images).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("worktree done would-remove"))
		Expect(out.String()).To(ContainSubstring("worktree dirty keep"))
		Expect(out.String()).To(ContainSubstring("worktree broken report"))
		Expect(out.String()).To(ContainSubstring("claim #77 report"))
		Expect(out.String()).To(ContainSubstring("dry run"))
	})

	It("with --apply removes the clean worktree, frees its branch, and keeps the rest", func() {
		Expect(run(dep, Options{Apply: true}, out, &bytes.Buffer{})).To(Equal(0))
		Expect(g.removed).To(Equal([]string{root + "/done"}))
		Expect(g.forced).To(BeEmpty())
		Expect(g.patched).To(BeEmpty())
		// feat/1-done lost its worktree in this run; feat/2-dirty and feat/3-open still have theirs.
		Expect(g.deleted).To(ConsistOf("feat/1-done", "feat/9-gone"))
	})

	It("with --apply --force-dirty saves a patch before it removes the dirty worktree", func() {
		Expect(run(dep, Options{Apply: true, ForceDirty: true, PatchDir: "/tmp/kw/.patches"}, out, &bytes.Buffer{})).To(Equal(0))
		Expect(g.patched).To(Equal([]string{root + "/dirty"}))
		Expect(g.forced).To(Equal([]string{root + "/dirty"}))
		Expect(out.String()).To(ContainSubstring("patch=/tmp/kw/.patches/dirty"))
	})

	It("does not remove the dirty worktree when the patch fails", func() {
		g.patchErr = errors.New("disk full")
		Expect(run(dep, Options{Apply: true, ForceDirty: true}, out, &bytes.Buffer{})).To(Equal(1))
		Expect(g.forced).To(BeEmpty())
		Expect(out.String()).To(ContainSubstring("failed"))
	})

	It("goes on after a failed removal and exits 1", func() {
		g.removeErr = map[string]error{root + "/done": errors.New("wt: refusing")}
		Expect(run(dep, Options{Apply: true}, out, &bytes.Buffer{})).To(Equal(1))
		Expect(g.deleted).To(ConsistOf("feat/9-gone"))
	})

	It("removes nothing when gh fails", func() {
		dep.Forge = fakeForge{err: errors.New("no network")}
		Expect(run(dep, Options{Apply: true}, out, &bytes.Buffer{})).To(Equal(0))
		Expect(g.removed).To(BeEmpty())
		Expect(g.deleted).To(BeEmpty())
		Expect(out.String()).To(ContainSubstring("unknown"))
	})

	It("runs Docker only with the flag, and refuses while a container runs", func() {
		Expect(run(dep, Options{Apply: true}, out, &bytes.Buffer{})).To(Equal(0))
		Expect(dk.containers + dk.images).To(Equal(0))
		dk.facts.Running = []string{"web"}
		Expect(run(dep, Options{Apply: true, WithDocker: true}, &bytes.Buffer{}, &bytes.Buffer{})).To(Equal(0))
		Expect(dk.containers + dk.images).To(Equal(0))
		Expect(run(dep, Options{Apply: true, WithDocker: true, ForceDocker: true}, &bytes.Buffer{}, &bytes.Buffer{})).To(Equal(0))
		Expect(dk.containers).To(Equal(1))
		Expect(dk.images).To(Equal(1))
	})

	It("prints one JSON array with --json", func() {
		Expect(run(dep, Options{JSON: true}, out, &bytes.Buffer{})).To(Equal(0))
		var decs []Decision
		Expect(json.Unmarshal(out.Bytes(), &decs)).To(Succeed())
		Expect(decs).NotTo(BeEmpty())
		Expect(decs[0].Kind).To(Equal("worktree"))
	})
})
