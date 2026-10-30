// SPDX-License-Identifier: Apache-2.0

package scrub_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/tracing/scrub"
)

// chatJSON builds a gen_ai.input.messages-shaped value of about size bytes: prose with the odd
// email, path, hash and URL, as a coding agent's context looks.
func chatJSON(size int) []byte {
	para := "Please look at internal/tracing/scrub/scrub.go (commit 3f2b8c1e9a4d4e7b8c215d6f7a8b9c0d1e2f3a4b) " +
		"and fix the failing test; see https://example.com/search?q=go+test&page=2 and mail jane.doe@example.com. " +
		"The run took 1234 ms and used 56789 tokens at $0.0123. "
	var msgs []map[string]any
	total := 0
	for i := 0; total < size; i++ {
		text := strings.Repeat(para, 3)
		msgs = append(msgs, map[string]any{"role": []string{"user", "assistant", "tool"}[i%3], "content": fmt.Sprintf("%d: %s", i, text)})
		total += len(text) + 40
	}
	b, _ := json.Marshal(msgs)
	return b
}

func benchSecrets() *scrub.Secrets {
	var v []string
	for i := 0; i < 50; i++ {
		v = append(v, fmt.Sprintf("koinon_test_not-a-real-secret-%02d", i))
	}
	return scrub.NewSecrets(v...)
}

func BenchmarkJSON16KiB(b *testing.B) {
	in := chatJSON(16 << 10)
	for _, lvl := range []scrub.Level{scrub.LevelFull, scrub.LevelRedacted} {
		b.Run(lvl.String(), func(b *testing.B) {
			s := scrub.New(scrub.Config{Level: lvl, Secrets: benchSecrets()})
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := s.JSON(in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkContent16KiB(b *testing.B) {
	in := string(chatJSON(64 << 10)) // scrub, then cut to the default limit
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: benchSecrets()})
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Content(in, 0)
	}
}

func BenchmarkTextShortAttribute(b *testing.B) {
	s := scrub.New(scrub.Config{Level: scrub.LevelRedacted, Secrets: benchSecrets()})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Text("gen_ai.request.model")
	}
}

func BenchmarkTruncateJSON16KiB(b *testing.B) {
	in := chatJSON(64 << 10)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := scrub.TruncateJSON(in, 0); err != nil {
			b.Fatal(err)
		}
	}
}
