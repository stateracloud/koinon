// SPDX-License-Identifier: Apache-2.0

package runwatch_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/runwatch"
)

// recorder is an Execer that keeps the NOTIFY payloads it was given.
type recorder struct {
	mu       sync.Mutex
	payloads []string
}

func (r *recorder) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, args[1].(string))
	return pgconn.CommandTag{}, nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.payloads...)
}

func uuid(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func update(text string) []byte {
	b, _ := json.Marshal(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	return b
}

var _ = Describe("live chunks", func() {
	It("keeps every payload under the NOTIFY cap, whatever the text", func() {
		// Quotes and control characters grow six-fold when JSON-escaped; 200 KB of them is the worst case.
		text := strings.Repeat("\"\x01é", 70000)
		payloads := runwatch.Payloads([]runwatch.Chunk{{Kind: "acp.agent_message_chunk", Text: text}})
		Expect(payloads).NotTo(BeEmpty())
		var back strings.Builder
		for _, p := range payloads {
			Expect(len(p)+74).To(BeNumerically("<", 8000), "with the '<org>/<run>/' prefix")
			var cs []runwatch.Chunk
			Expect(json.Unmarshal([]byte(p), &cs)).To(Succeed())
			for _, c := range cs {
				back.WriteString(c.Text)
			}
		}
		Expect(back.String()).To(Equal(text), "split on rune boundaries, in order, nothing lost")
	})

	It("batches what arrives within an interval into one notification, joining neighbours of a kind", func() {
		db := &recorder{}
		org, run := uuid(1), uuid(2)
		pub := runwatch.NewChunkPublisher(db, org, run, time.Hour, nil)
		for _, t := range []string{"a", "b", "c"} {
			pub.Add("acp.agent_message_chunk", update(t))
		}
		pub.Add("acp.agent_thought_chunk", update("t"))
		pub.Add("acp.tool_call", []byte(`{"title":"x"}`)) // no text: not live
		pub.Close()                                       // sends what is queued

		got := db.got()
		Expect(got).To(HaveLen(1))
		prefix := runwatch.Key(org, run) + "/"
		Expect(got[0]).To(HavePrefix(prefix))
		Expect(got[0][len(prefix):]).To(MatchJSON(`[{"k":"acp.agent_message_chunk","t":"abc"},{"k":"acp.agent_thought_chunk","t":"t"}]`))
	})

	It("sends on its own while the agent types", func() {
		db := &recorder{}
		pub := runwatch.NewChunkPublisher(db, uuid(1), uuid(2), 10*time.Millisecond, nil)
		DeferCleanup(pub.Close)
		pub.Add("acp.agent_message_chunk", update("hi"))
		Eventually(db.got, 2*time.Second).Should(HaveLen(1))
	})
})
