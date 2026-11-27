// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// DefaultLogDir is the directory of the job logs. The upstream process tracker opens the log files
// there as root, and it follows symlinks (os.OpenFile with O_CREATE and O_TRUNC, and MkdirAll).
// The payload must not be able to write the directory, or it could plant a symlink and make the
// root front truncate or create a file anywhere (issue #1503). On the vm tier /run is read-only for
// the payload (the policy lists it in read_exec, and Landlock is in require mode). On the gVisor
// tiers Landlock is best effort: there the control is the non-root payload (uid 10001) and a
// directory that root owns with mode 0750, under a /run that root owns. The upstream default,
// /var/log/ate-jobs, is under /var/log, which the payload can write.
const DefaultLogDir = "/run/koinon/ate-jobs"

// PrepareLogDir makes dir (mode 0750) and checks it, before any payload exists. It walks the path
// from "/" with one openat(O_NOFOLLOW|O_DIRECTORY) per part and makes a missing part with mkdirat
// on the open parent, so it never follows a symlink and has no check-then-open gap. It fails when a
// part is a symlink or not a directory, when a part is owned by a user other than root or the
// current user, or when a part can be written by group or others (a parent with the sticky bit is
// allowed to be, as /tmp is). The last part must also not be writable by group or others.
func PrepareLogDir(dir string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("log dir %q is not an absolute path", dir)
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("log dir: open /: %w", err)
	}
	for i, name := range parts {
		last := i == len(parts)-1
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, name, 0o750); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(fd)
				return fmt.Errorf("log dir %s: make %q: %w", dir, name, err)
			}
			next, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("log dir %s: open %q without following a symlink: %w", dir, name, err)
		}
		fd = next
		if err := checkPart(fd, dir, name, last); err != nil {
			_ = unix.Close(fd)
			return err
		}
	}
	return unix.Close(fd)
}

// checkPart checks one open directory of the log path.
func checkPart(fd int, dir, name string, last bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("log dir %s: %w", dir, err)
	}
	if st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("log dir %s: %q is owned by uid %d", dir, name, st.Uid)
	}
	if last && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("log dir %s: owner is uid %d, want uid %d", dir, st.Uid, os.Geteuid())
	}
	sticky := st.Mode&unix.S_ISVTX != 0
	if st.Mode&0o022 != 0 && (last || !sticky) {
		return fmt.Errorf("log dir %s: %q has mode %o, which lets group or others write", dir, name, st.Mode&0o7777)
	}
	return nil
}
