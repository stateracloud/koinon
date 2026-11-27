// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"io"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("reaper.scan", func() {
	var (
		r       *reaper
		zombies []zombie
		reaped  []int
		t0      time.Time
	)
	BeforeEach(func() {
		t0 = time.Unix(1000, 0)
		zombies, reaped = nil, nil
		r = newReaper(time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
		r.list = func() ([]zombie, error) { return zombies, nil }
		r.reap = func(pid int) bool { reaped = append(reaped, pid); return true }
	})

	It("does not take a zombie in its grace period, so cmd.Wait keeps the exit status", func() {
		zombies = []zombie{{PID: 10, Start: 1}}
		Expect(r.scan(t0)).To(Equal(time.Second))
		Expect(r.scan(t0.Add(400 * time.Millisecond))).To(Equal(600 * time.Millisecond))
		Expect(reaped).To(BeEmpty())
	})

	It("takes a zombie that is still there after the grace period, and only that PID", func() {
		zombies = []zombie{{PID: 10, Start: 1}}
		r.scan(t0)
		zombies = append(zombies, zombie{PID: 11, Start: 2})
		Expect(r.scan(t0.Add(time.Second))).To(Equal(time.Second))
		Expect(reaped).To(Equal([]int{10}))
		Expect(r.seen).To(HaveKey(zombie{PID: 11, Start: 2}))
		Expect(r.seen).NotTo(HaveKey(zombie{PID: 10, Start: 1}))
	})

	It("forgets a zombie that went away and treats a reused PID as a new zombie", func() {
		zombies = []zombie{{PID: 10, Start: 1}}
		r.scan(t0)
		zombies = nil
		Expect(r.scan(t0.Add(500 * time.Millisecond))).To(BeZero())
		zombies = []zombie{{PID: 10, Start: 9}}
		Expect(r.scan(t0.Add(2 * time.Second))).To(Equal(time.Second))
		Expect(reaped).To(BeEmpty())
	})
})
