// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/stateracloud/koinon/internal/db/store"
)

// fakeAgentd answers the storage probes the keys commands write (home_repository_probes), as
// koinon-agentd does with the bucket's credentials, which this package never has (ADR 0032):
// answer decides each. It stops with the spec. The real answerer against a real bucket is in
// internal/agentd (probe_test.go).
func fakeAgentd(answer func(store.HomeRepositoryProbe) (outcome, message string)) {
	GinkgoHelper()
	answering.Store(&answer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	DeferCleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			rows, err := pool.Query(ctx, `SELECT org_id, id, agent_id, repository_epoch FROM home_repository_probes WHERE answered_at IS NULL`)
			if err == nil {
				var due []store.HomeRepositoryProbe
				for rows.Next() {
					var p store.HomeRepositoryProbe
					if rows.Scan(&p.OrgID, &p.ID, &p.AgentID, &p.RepositoryEpoch) == nil {
						due = append(due, p)
					}
				}
				rows.Close()
				for _, p := range due {
					fn := answering.Load()
					if fn == nil {
						break
					}
					outcome, msg := (*fn)(p)
					_, _ = pool.Exec(ctx, `UPDATE home_repository_probes SET answered_at = now(), outcome = $3, message = $4
						WHERE org_id = $1 AND id = $2 AND answered_at IS NULL`, p.OrgID, p.ID, outcome, msg)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
}

// answering is what the running fake agentd answers with; nil: it does not answer (agentd is down).
var answering atomic.Pointer[func(store.HomeRepositoryProbe) (string, string)]

// answerWith changes what the fake agentd answers with in the spec; nil makes it silent.
func answerWith(answer func(store.HomeRepositoryProbe) (outcome, message string)) {
	if answer == nil {
		answering.Store(nil)
		return
	}
	answering.Store(&answer)
}

// freeStorage is an agentd whose bucket holds nothing.
func freeStorage() {
	GinkgoHelper()
	fakeAgentd(func(store.HomeRepositoryProbe) (string, string) { return "free", "" })
}
