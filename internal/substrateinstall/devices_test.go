// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

// The install preflight checks the microVM node devices only when the install has the microvm sandbox class
// (issue #1091: the kind leg on a runner without KVM installs gvisor only).

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

// countingDevices counts the device probes.
type countingDevices struct{ calls int }

func (c *countingDevices) ProbeDevices(context.Context) error { c.calls++; return nil }

func TestUsesMicroVMFollowsTheInstalledSandboxClasses(t *testing.T) {
	g := NewWithT(t)
	g.Expect(substrateinstall.Profile{}.UsesMicroVM()).To(BeTrue(), "the default classes are gvisor,microvm")
	g.Expect(substrateinstall.Profile{SandboxClasses: "gvisor, microvm"}.UsesMicroVM()).To(BeTrue())
	g.Expect(substrateinstall.Profile{SandboxClasses: "microvm"}.UsesMicroVM()).To(BeTrue())
	g.Expect(substrateinstall.Profile{SandboxClasses: "gvisor"}.UsesMicroVM()).To(BeFalse())
	g.Expect(substrateinstall.Profile{Platform: substrateinstall.PlatformGeneric}.UsesMicroVM()).To(BeFalse())
}

func TestProbesTheMicroVMDevicesOnlyWithTheMicroVMClass(t *testing.T) {
	for name, tc := range map[string]struct {
		classes string
		probes  bool
	}{
		"gvisor only needs no microVM device": {"gvisor", false},
		"the default classes include microvm": {"", true},
		"microvm needs the devices":           {"microvm", true},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			i, _, _ := setup("37", true, nil)
			devices := &countingDevices{}
			i.Checker.Devices = devices
			i.Checker.Config.Substrate.VMTier = true
			i.Profile.SandboxClasses = tc.classes
			_ = i.Install(t.Context()) // later steps fail: Substrate itself is not in the fake
			g.Expect(devices.calls > 0).To(Equal(tc.probes))
		})
	}
}
