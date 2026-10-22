// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

// SelfProbe is what the probe pod runs: it reads its own seccomp state and tries keyctl.
func SelfProbe() (ProbeResult, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ProbeResult{}, err
	}
	seccomp, filters, err := parseStatus(string(status))
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Seccomp: seccomp, Filters: filters, Keyctl: keyctl()}, nil
}

// keyctl calls keyctl(KEYCTL_GET_KEYRING_ID, KEY_SPEC_SESSION_KEYRING, 0), which only reads
// the session keyring's ID. RuntimeDefault denies it with EPERM; OpenShell's filter does not.
func keyctl() string {
	const getKeyringID = 0
	// KEY_SPEC_SESSION_KEYRING; the kernel takes key serials as signed ints.
	sessionKeyring := -3
	_, _, errno := syscall.Syscall(syscall.SYS_KEYCTL, getKeyringID, uintptr(sessionKeyring), 0) //nolint:gosec // signed key serial
	switch {
	case errno == 0:
		return "allowed"
	case errors.Is(errno, syscall.EPERM):
		return "EPERM"
	case errors.Is(errno, syscall.ENOSYS):
		return "ENOSYS"
	default:
		return "errno" + strconv.Itoa(int(errno))
	}
}
