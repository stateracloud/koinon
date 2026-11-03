// SPDX-License-Identifier: Apache-2.0

package remotekms_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/remotekms"
)

func TestRemoteKMS(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "remotekms Suite")
}

var _ = Describe("the frame", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	It("round-trips a key for the same associated data and clears the plaintext", func() {
		plain := remotekms.Frame(kek, aad)
		Expect(bytes.Contains(plain, aad)).To(BeFalse(), "only a digest of the associated data is framed")
		got, err := remotekms.Unframe(plain, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		Expect(plain).To(Equal(make([]byte, len(plain))))
	})

	DescribeTable("refuses as not opening", func(plain func() []byte) {
		_, err := remotekms.Unframe(plain(), aad)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	},
		Entry("other associated data", func() []byte { return remotekms.Frame(kek, []byte("koinon-test-other")) }),
		Entry("another version", func() []byte { p := remotekms.Frame(kek, aad); p[0] = 2; return p }),
		Entry("no key", func() []byte { return remotekms.Frame(nil, aad) }),
		Entry("too short", func() []byte { return []byte{1, 2, 3} }),
		Entry("nothing", func() []byte { return nil }),
	)
})

var _ = Describe("Unreachable", func() {
	It("wraps once in secretstore.ErrUnreachable and keeps the cause", func() {
		cause := errors.New("connection refused")
		err := remotekms.Unreachable("kms decrypt", cause)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err).To(MatchError(cause))
		again := remotekms.Unreachable("outer", err)
		Expect(again.Error()).To(Equal("outer: " + err.Error()))
	})
})

var _ = Describe("Bound", func() {
	It("applies the default when no timeout is set and keeps an earlier caller deadline", func() {
		ctx, cancel := remotekms.Bound(context.Background(), 0)
		defer cancel()
		dl, ok := ctx.Deadline()
		Expect(ok).To(BeTrue())
		Expect(time.Until(dl)).To(BeNumerically("~", remotekms.DefaultTimeout, time.Second))

		parent, pcancel := context.WithTimeout(context.Background(), time.Second)
		defer pcancel()
		ctx, cancel = remotekms.Bound(parent, time.Hour)
		defer cancel()
		dl, _ = ctx.Deadline()
		Expect(time.Until(dl)).To(BeNumerically("<=", time.Second))
	})
})
