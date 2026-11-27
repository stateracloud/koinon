// SPDX-License-Identifier: Apache-2.0

package guestfront_test

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// subreaperFlag reads PR_GET_CHILD_SUBREAPER of the test process. The kernel writes the value to
// the address in the second argument.
func subreaperFlag() bool {
	var v int32
	err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&v)), 0, 0, 0)
	return err == nil && v == 1
}

func setSubreaper(on bool) {
	var v uintptr
	if on {
		v = 1
	}
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, v, 0, 0, 0)
}
