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
// allowed to be, as /tmp is). The last part must also give others no access and its group no write:
// a directory made earlier with mode 0755 would let uid 10001 read the job logs on gVisor, so it is
// refused and not accepted (issue #1516).
func PrepareLogDir(dir string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("log dir %q is not an absolute path", dir)
	}
	if dir == "/" {
		return errors.New("log dir \"/\" is the root directory: it must be a directory below it")
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("log dir: open /: %w", err)
	}
	for i, name := range parts {
		last := i == len(parts)-1
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		made := false
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, name, 0o750); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(fd)
				return fmt.Errorf("log dir %s: make %q: %w", dir, name, err)
			}
			made = err == nil // an EEXIST is a directory that someone else made
			next, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			// A new directory can take the group of its parent (BSD, macOS, a setgid parent). The front owns
			// the directory it made, so it sets the group to its own.
			if made && last && err == nil {
				if cerr := unix.Fchown(next, -1, os.Getegid()); cerr != nil {
					_ = unix.Close(next)
					_ = unix.Close(fd)
					return fmt.Errorf("log dir %s: set the group of %q: %w", dir, name, cerr)
				}
			}
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
	if last && st.Mode&0o070 != 0 && int(st.Gid) != os.Getegid() {
		// The group can read: a group other than the front's could be the payload group.
		return fmt.Errorf("log dir %s: %q has gid %d, want gid %d", dir, name, st.Gid, os.Getegid())
	}
	if last && st.Mode&0o027 != 0 {
		return fmt.Errorf("log dir %s: %q has mode %o, which lets others access it or the group write; want 0750 or stricter", dir, name, st.Mode&0o7777)
	}
	return nil
}
