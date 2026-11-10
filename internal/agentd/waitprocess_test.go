// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"io"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
)

// stuckProcess is a harness blocked writing to a pipe nobody reads: Wait returns only once the
// stream is closed, or never when stubborn is set.
type stuckProcess struct {
	closed   chan struct{}
	once     sync.Once
	stubborn bool
	// closeBlocks makes Close hang, as an OpenShell exec's does while its command runs.
	closeBlocks bool
}

func newStuck(stubborn bool) *stuckProcess {
	return &stuckProcess{closed: make(chan struct{}), stubborn: stubborn}
}

func (p *stuckProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (p *stuckProcess) Stdout() io.Reader     { return strings.NewReader("") }
func (p *stuckProcess) Close() error {
	if p.closeBlocks {
		select {}
	}
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *stuckProcess) Wait() (int, error) {
	if p.stubborn {
		select {}
	}
	<-p.closed
	return 0, io.ErrUnexpectedEOF
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriteCloser) Close() error                { return nil }

var _ = Describe("waiting for a disconnected harness", func() {
	It("closes the stream of a harness that does not exit, so the wait ends", func() {
		start := time.Now()
		_, err := agentd.WaitProcess(newStuck(false), 100*time.Millisecond)
		Expect(err).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})

	It("gives up on a harness that ignores the close", func() {
		start := time.Now()
		_, err := agentd.WaitProcess(newStuck(true), 100*time.Millisecond)
		Expect(err).To(MatchError(ContainSubstring("did not exit")))
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})

	It("is bounded even when Close blocks", func() {
		p := newStuck(true)
		p.closeBlocks = true
		start := time.Now()
		_, err := agentd.WaitProcess(p, 100*time.Millisecond)
		Expect(err).To(MatchError(ContainSubstring("did not exit")))
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})
})

var _ = Describe("the reason of a harness line that is too long", func() {
	It("ends without a colon when the harness wrote nothing to stderr", func() {
		Expect(agentd.LineTooLongReason("  \n")).To(HaveSuffix("without a newline"))
	})

	It("adds the tail of stderr when there is one", func() {
		Expect(agentd.LineTooLongReason("boom\n")).To(HaveSuffix("without a newline: boom"))
	})
})
