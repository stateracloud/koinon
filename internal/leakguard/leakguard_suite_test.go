// SPDX-License-Identifier: Apache-2.0

package leakguard_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/leakguard"
)

func TestLeakguard(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Leakguard Suite")
}

// testKey is an obviously fake fingerprint key.
var testKey = []byte("koinon_test_not-a-real-key-0001")

var testScope = leakguard.Scope{Org: "org-test", Agent: "agent-test", Generation: "gen-1"}

func newFingerprinter(key []byte) *leakguard.Fingerprinter {
	fp, err := leakguard.NewFingerprinter(key, leakguard.DefaultParams())
	if err != nil {
		panic(err)
	}
	return fp
}

// taintedStore returns a store holding the fingerprints of private under testScope.
func taintedStore(fp *leakguard.Fingerprinter, private ...string) *leakguard.MemoryStore {
	st := leakguard.NewMemoryStore(leakguard.MemoryStoreOptions{})
	for _, p := range private {
		if err := st.Add(context.Background(), testScope, fp.Winnow(p)); err != nil {
			panic(err)
		}
	}
	return st
}

// percentAll percent-encodes every byte of s.
func percentAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func b64(s string) string    { return base64.StdEncoding.EncodeToString([]byte(s)) }
func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func hexOf(s string) string  { return hex.EncodeToString([]byte(s)) }
