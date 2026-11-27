// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"io"
	"log/slog"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("reaper.scan", func() {
	var (
		r       *reaper
		zombies []zombie
		owned   map[int]bool
		reaped  []int
		logs    logSink
	)
	BeforeEach(func() {
		zombies, reaped, owned, logs = nil, nil, map[int]bool{}, logSink{}
		r = &reaper{
			logger:    slog.New(slog.NewTextHandler(&logs, nil)),
			list:      func() ([]zombie, error) { return zombies, nil },
			reap:      func(pid int) bool { reaped = append(reaped, pid); return true },
			owns:      func(pid int) bool { return owned[pid] },
			exclusive: func() func() { return func() {} },
		}
	})

	It("never takes the zombie of a process that the front started, however late its wait is", func() {
		zombies = []zombie{{PID: 10}}
		owned[10] = true
		for range 5 { // a slow Wait: the zombie stays for many scans
			Expect(r.scan()).To(BeTrue(), "the reaper looks again later")
		}
		Expect(reaped).To(BeEmpty())
	})

	It("takes a zombie that the front did not start, at once, and only that PID", func() {
		zombies = []zombie{{PID: 10}, {PID: 11}}
		owned[10] = true
		Expect(r.scan()).To(BeTrue())
		Expect(reaped).To(Equal([]int{11}))
		Expect(logs.String()).To(ContainSubstring("level=INFO"))
		Expect(logs.String()).To(ContainSubstring("reaped an orphan"))
	})

	It("takes a zombie when its PID was owned and the wait is over (the PID was reused)", func() {
		zombies = []zombie{{PID: 10}}
		Expect(r.scan()).To(BeFalse())
		Expect(reaped).To(Equal([]int{10}))
	})

	It("holds the start gate during a scan", func() {
		var mu sync.Mutex
		held := false
		r.exclusive = func() func() { mu.Lock(); held = true; return func() { held = false; mu.Unlock() } }
		r.list = func() ([]zombie, error) { Expect(held).To(BeTrue()); return nil, nil }
		r.scan()
		Expect(held).To(BeFalse())
	})
})

// logSink is a log sink that a spec reads back.
type logSink struct {
	mu sync.Mutex
	b  []byte
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}
func (s *logSink) String() string { s.mu.Lock(); defer s.mu.Unlock(); return string(s.b) }

var _ io.Writer = (*logSink)(nil)
