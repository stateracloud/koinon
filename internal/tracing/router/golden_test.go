// SPDX-License-Identifier: Apache-2.0

package router_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stateracloud/koinon/internal/tracing/router"
)

// goldenJSON renders what a backend received: the resource and scope of the first request, and every
// span in a stable order. Moving the pinned conventions to a newer commit must change these files on
// purpose (ADR 0074, decision 3); UPDATE_GOLDEN=1 rewrites them.
func goldenJSON(b *backend) []byte {
	b.mu.Lock()
	first := b.reqs[0].ResourceSpans[0]
	b.mu.Unlock()
	spans := []json.RawMessage{}
	for _, sp := range b.spans() {
		raw, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(sp)
		Expect(err).NotTo(HaveOccurred())
		spans = append(spans, raw)
	}
	res, err := protojson.Marshal(first.Resource)
	Expect(err).NotTo(HaveOccurred())
	doc := map[string]any{
		"resource":  json.RawMessage(res),
		"schemaUrl": first.SchemaUrl,
		"scope":     first.ScopeSpans[0].Scope.Name,
		"spans":     spans,
	}
	// protojson output is deliberately unstable between runs; re-encoding through encoding/json sorts
	// the keys and fixes the whitespace.
	compact, err := json.Marshal(doc)
	Expect(err).NotTo(HaveOccurred())
	var canon any
	Expect(json.Unmarshal(compact, &canon)).To(Succeed())
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	Expect(enc.Encode(canon)).To(Succeed())
	return out.Bytes()
}

func checkGolden(name string, got []byte) {
	path := filepath.Join("testdata", name+".golden.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		Expect(os.MkdirAll("testdata", 0o755)).To(Succeed())
		Expect(os.WriteFile(path, got, 0o600)).To(Succeed())
		return
	}
	want, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "run with UPDATE_GOLDEN=1 to create %s", path)
	Expect(string(got)).To(Equal(string(want)), "the emitted spans changed; if on purpose, UPDATE_GOLDEN=1")
}

var _ = Describe("Exported spans (golden)", func() {
	DescribeTable("one turn: a root, a tool call and two model calls",
		func(name, level string, platform bool) {
			be := newBackend()
			e := newEnv(func(c *router.Config) { c.Linger = 20 * time.Millisecond }, secretsFor(orgA))
			e.routes.set(uid(orgA), be.route("d1", platform, level))
			tr := newTurn(e, orgA, runA, 1)
			e.clock.SetNow(t0.Add(11 * time.Second))
			e.store.setCalls([]router.ModelCall{call("req-1", 2*time.Second, 0.25), call("req-2", 6*time.Second, 0.125)})
			e.post(tr.root(true), tr.tool(true))
			Eventually(func() int { return e.r.HeldRoots() }).Should(Equal(1))
			finishTurn(e)
			Eventually(func() int { return len(be.spans()) }).Should(Equal(4))
			checkGolden(name, goldenJSON(be))
		},
		Entry("org copy at none", "turn_none", "none", false),
		Entry("org copy at redacted", "turn_redacted", "redacted", false),
		Entry("org copy at full", "turn_full", "full", false),
		Entry("platform copy at none", "turn_platform_none", "none", true),
		Entry("platform copy at redacted", "turn_platform_redacted", "redacted", true),
	)
})
