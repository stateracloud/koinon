// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/leakguard"
)

// benignArgs is a realistic tool call: a URL and a few kilobytes of prose with some encoded
// fragments, none of it tainted.
func benignArgs() []string {
	prose := strings.Repeat("Kubernetes 1.34 deprecates several beta APIs; see the release notes at https://kubernetes.io/docs/. ", 40)
	return []string{
		"https://github.com/kubernetes/kubernetes/releases/tag/v1.34.0?tab=assets",
		prose,
		"note=" + b64("a short encoded note about the release") + " id=" + hexOf("release-1.34"),
	}
}

func BenchmarkNormalise(b *testing.B) {
	s := strings.Repeat("The payroll export job runs nightly on the finance node. Ünïcødé ＦＵＬＬ. ", 64)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		leakguard.Normalise(s)
	}
}

func BenchmarkWinnow1MiB(b *testing.B) {
	fp := newFingerprinter(testKey)
	s := randomText(rand.New(rand.NewPCG(5, 6)), 1<<20)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		fp.Winnow(s)
	}
}

func BenchmarkTaintCheck(b *testing.B) {
	fp := newFingerprinter(testKey)
	st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{})
	if err := st.Add(context.Background(), testScope, fp.Winnow(randomText(rand.New(rand.NewPCG(7, 8)), 1<<20))); err != nil {
		b.Fatal(err)
	}
	checker := leakguard.TaintChecker{Store: st}
	args := benignArgs()
	n := 0
	for _, a := range args {
		n += len(a)
	}
	b.SetBytes(int64(n))
	for b.Loop() {
		d, err := checker.Check(context.Background(), testScope, fp, args)
		if err != nil || !d.Allow {
			b.Fatalf("%+v %v", d, err)
		}
	}
}

func BenchmarkCheckURL(b *testing.B) {
	p := leakguard.DefaultURLPolicy()
	for b.Loop() {
		p.CheckURL(leakguard.NewRunState(), "https://api.github.com/repos/cert-manager/cert-manager/releases?per_page=5")
	}
}
