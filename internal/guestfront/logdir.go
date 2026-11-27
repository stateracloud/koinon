// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DefaultLogDir is the directory of the job logs. The upstream process tracker opens the log files
// there as root, and it follows symlinks (os.OpenFile with O_CREATE and O_TRUNC, and MkdirAll).
// The payload must not be able to write the directory, or it could plant a symlink and make the
// root front truncate or create a file anywhere (issue #1503). /run is read-only for the payload
// on the vm tier (the policy lists it in read_exec), so the directory is safe by construction.
// The upstream default, /var/log/ate-jobs, is under /var/log, which the payload can write.
const DefaultLogDir = "/run/koinon/ate-jobs"

// PrepareLogDir makes dir (mode 0750, owned by the current user) and checks it. It fails when a
// part of the path is a symlink, when the directory is not a directory, when another user owns it
// or when group or others can write it. It must run before the payload starts, so no check races
// a payload. It opens with O_NOFOLLOW, so a symlink in the last part is refused by the kernel.
func PrepareLogDir(dir string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("log dir %q is not an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("log dir: %w", err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("log dir %s: open without following a symlink: %w", dir, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("log dir %s: %w", dir, err)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("log dir %s: owner is uid %d, want uid %d", dir, st.Uid, os.Geteuid())
	}
	if st.Mode&0o022 != 0 {
		return fmt.Errorf("log dir %s: mode %o lets group or others write", dir, st.Mode&0o7777)
	}
	// A symlink in a parent part leads to another place: the real path of the open directory must
	// be the path we were given.
	real, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return fmt.Errorf("log dir %s: %w", dir, err)
	}
	if real != dir {
		return fmt.Errorf("log dir %s: a symlink leads to %s", dir, real)
	}
	return nil
}
