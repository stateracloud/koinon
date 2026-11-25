// SPDX-License-Identifier: Apache-2.0

package koinongateway

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type fakeGuard struct{ err error }

func (f fakeGuard) Loaded() error { return f.err }

type fakeKeys struct{ ok bool }

func (f fakeKeys) Ready() (bool, error) { return f.ok, nil }

var _ = Describe("the readiness of the pod", func() {
	It("is not ready while the guard has not reached its database", func() {
		Expect(podReady(fakeGuard{err: errors.New("not loaded")}, fakeKeys{ok: true}, true)).To(BeFalse())
	})
	It("is ready when the guard, the keys and the config are", func() {
		Expect(podReady(fakeGuard{}, fakeKeys{ok: true}, true)).To(BeTrue())
	})
	It("is not ready without a config or without its keys", func() {
		Expect(podReady(fakeGuard{}, fakeKeys{ok: true}, false)).To(BeFalse())
		Expect(podReady(fakeGuard{}, fakeKeys{ok: false}, true)).To(BeFalse())
	})
	It("is ready with no guard and no key loader (a pod that runs the model path alone)", func() {
		Expect(podReady(nil, nil, true)).To(BeTrue())
	})
})
