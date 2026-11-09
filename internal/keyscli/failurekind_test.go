// SPDX-License-Identifier: Apache-2.0

package keyscli_test

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/keyscli"
	"github.com/stateracloud/koinon/internal/secretstore"
)

var _ = Describe("failureKind", func() {
	It("files an unsupported envelope format or algorithm under envelope", func() {
		err := fmt.Errorf("row: %w", secretstore.ErrUnsupportedFormat)
		Expect(keyscli.FailureKind(err)).To(Equal("envelope"))
	})
	It("keeps the other kinds apart", func() {
		Expect(keyscli.FailureKind(secretstore.ErrOpen)).To(Equal("open"))
		Expect(keyscli.FailureKind(errors.New("connection reset"))).To(Equal("database"))
	})
})
