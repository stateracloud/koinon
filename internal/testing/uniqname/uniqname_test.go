// SPDX-License-Identifier: Apache-2.0

package uniqname_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stateracloud/koinon/internal/testing/uniqname"
)

func TestSuffixIsUniqueUnderConcurrency(t *testing.T) {
	const workers, each = 16, 500
	seen := make(chan int64, workers*each)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				seen <- uniqname.Suffix()
			}
		})
	}
	wg.Wait()
	close(seen)
	got := map[int64]bool{}
	for s := range seen {
		if got[s] {
			t.Fatalf("suffix %d was returned twice", s)
		}
		got[s] = true
	}
}

func TestNameShape(t *testing.T) {
	a, b := uniqname.Name("kesa"), uniqname.Name("kesa")
	if a == b {
		t.Fatalf("two names are equal: %s", a)
	}
	if !strings.HasPrefix(a, "kesa-") {
		t.Fatalf("name %q lacks the prefix", a)
	}
	// "kesag-" (6) plus at most 11 digits must fit the 19 characters of an OpenShell sandbox name.
	if n := len(uniqname.Name("kesag")); n > 19 {
		t.Fatalf("name of %d characters is above the OpenShell limit of 19", n)
	}
}
