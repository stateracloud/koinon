// SPDX-License-Identifier: Apache-2.0

//go:build linux

package guestlaunch

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// Exec applies the policy to the calling process in this order and then replaces the process with
// argv (looked up on PATH when it has no slash): no_new_privs, the user drop, the capability
// allowlist, Landlock, seccomp. The order matters: the user drop needs CAP_SETUID and the capability
// drop needs CAP_SETPCAP, so both come before the filters, and Landlock and seccomp need
// no_new_privs. Exec only returns an error; on success the payload runs in its place.
func Exec(p Policy, argv, env []string, log *slog.Logger) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if len(argv) == 0 {
		return errors.New("guest launcher: no command to run")
	}
	path, err := lookPath(argv[0], env)
	if err != nil {
		return err
	}
	// Privilege state is per thread: do everything on one thread and exec from it.
	runtime.LockOSThread()

	// No descriptor above stderr reaches the payload: a socket the guest server left open without
	// CLOEXEC (its listener and connections among them) would otherwise pass into the payload with the exec.
	if err := closeInheritedFds(); err != nil {
		return err
	}
	log.Info("guest launcher: descriptors above 2 close on exec")

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("guest launcher: set no_new_privs: %w", err)
	}
	log.Info("guest launcher: no_new_privs set")

	// The bounding set is shrunk first, while the process is still root with CAP_SETPCAP: after the
	// user drop the effective set is empty and PR_CAPBSET_DROP would fail.
	boundingDropped := dropBoundingSet(p.Capabilities.Allow)
	if err := dropUser(p.User, log); err != nil {
		return err
	}
	if err := applyCapabilities(p.Capabilities.Allow, p.User, boundingDropped, log); err != nil {
		return err
	}
	if err := applyLandlock(p.Landlock, log); err != nil {
		return err
	}
	if err := applySeccomp(p.Seccomp, log); err != nil {
		return err
	}
	log.Info("guest launcher: starting payload", "path", path)
	return syscall.Exec(path, argv, env) //nolint:gosec // the launcher's purpose: exec the payload the policy names
}

// closeInheritedFds marks every descriptor from 3 up close-on-exec, so the execve that starts the
// payload closes them all at once. Marking, not closing: the Go runtime holds descriptors of its own
// (the netpoller's epoll), and closing one under it before the exec can crash the launcher. It uses
// close_range(3, ~0U, CLOSE_RANGE_CLOEXEC) (Linux 5.11); on a kernel without it (ENOSYS, EPERM from an outer
// seccomp profile, or EINVAL for the flag on 5.9 and 5.10, or a gVisor that lacks it) it sets FD_CLOEXEC on each descriptor
// /proc/self/fd lists. It fails closed: a descriptor it cannot mark stops the launch.
func closeInheritedFds() error {
	err := unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)
	if err == nil {
		return nil
	}
	// EPERM: an outer seccomp profile may return it for a syscall it does not know. The fallback
	// still fails closed.
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("guest launcher: close_range: %w", err)
	}
	return cloexecFromProc()
}

func cloexecFromProc() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("guest launcher: close_range unavailable and /proc/self/fd unreadable: %w", err)
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		// The descriptor of the directory read above is already closed; EBADF is that one.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil && !errors.Is(err, unix.EBADF) {
			return fmt.Errorf("guest launcher: set close-on-exec on fd %d: %w", fd, err)
		}
	}
	return nil
}

func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathVar := "/usr/local/bin:/usr/bin:/bin"
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			pathVar = v
		}
	}
	for _, dir := range strings.Split(pathVar, ":") {
		c := dir + "/" + name
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("guest launcher: %q not found in PATH", name)
}

// dropUser switches to the policy's uid and gid. In require mode (the default and the gVisor tiers,
// issue #1387) a failed drop stops the start: the payload never runs as root. In best_effort mode
// (the vm tier until issue #1462) a failed drop is logged and the launcher continues with the current
// user: the microVM guest of Substrate v1.0.1 gave payloads no CAP_SETUID or CAP_SETGID (issue #1087).
func dropUser(u UserPolicy, log *slog.Logger) error {
	if u.Mode == ModeOff || u.UID == 0 {
		return nil
	}
	if os.Geteuid() == int(u.UID) && os.Getegid() == int(u.GID) {
		return nil
	}
	// Keep the permitted set across the uid change so applyCapabilities can raise the allowlist.
	_ = unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0)
	err := setIDs(u)
	if err == nil {
		log.Info("guest launcher: dropped to user", "uid", u.UID, "gid", u.GID)
		return nil
	}
	if u.Mode == ModeRequire {
		return fmt.Errorf("guest launcher: user drop to %d:%d required but failed: %w", u.UID, u.GID, err)
	}
	log.Warn("guest launcher: user drop failed, continuing as the current user with every other control applied (best_effort; issue #1462: the guest lacks CAP_SETUID/CAP_SETGID)",
		"uid", u.UID, "gid", u.GID, "error", err.Error(), "current_uid", os.Geteuid())
	return nil
}

func setIDs(u UserPolicy) error {
	// Group first: after setresuid the process can no longer change its groups.
	if err := unix.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setresgid(int(u.GID), int(u.GID), int(u.GID)); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := unix.Setresuid(int(u.UID), int(u.UID), int(u.UID)); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	return nil
}

func lastCap() int {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return len(capNames) - 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return len(capNames) - 1
	}
	return n
}

func capMask(allow []string) (keep [2]uint32) {
	for _, n := range allow {
		c, _ := capNumber(n)
		keep[c/32] |= 1 << (uint(c) % 32)
	}
	return keep
}

// dropBoundingSet removes every capability outside the allowlist from the bounding set. It needs
// CAP_SETPCAP in the effective set and returns false (nothing dropped) when the process lacks it,
// as in a guest that gives payloads only a few capabilities.
func dropBoundingSet(allow []string) bool {
	keep := capMask(allow)
	var data [2]unix.CapUserData
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&hdr, &data[0]); err != nil || data[0].Effective&(1<<unix.CAP_SETPCAP) == 0 {
		return false
	}
	for c := 0; c <= lastCap(); c++ {
		if keep[c/32]&(1<<(uint(c)%32)) != 0 {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return false
		}
	}
	return true
}

// applyCapabilities reduces permitted, effective and inheritable to the allowlist (the bounding
// set was dropped earlier). After a user drop the kernel has cleared the effective set but kept
// the permitted set (PR_SET_KEEPCAPS), so the allowlist is restored as effective and inheritable
// and, for a non-root user, raised as ambient so it survives the exec. A capability the payload
// does not hold cannot be raised: that is logged, and it fails the launch in require mode.
func applyCapabilities(allow []string, u UserPolicy, boundingDropped bool, log *slog.Logger) error {
	keep := capMask(allow)

	var data [2]unix.CapUserData
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("guest launcher: capget: %w", err)
	}
	// Clear ambient first; raise the allowlist again below when it applies.
	_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)

	nonRoot := os.Geteuid() != 0
	for i := range data {
		data[i].Permitted &= keep[i]
		if nonRoot {
			// Effective was cleared by the uid change; the permitted allowlist is made effective
			// again, and inheritable carries it so the ambient raise below is allowed.
			data[i].Effective = data[i].Permitted
			data[i].Inheritable = (data[i].Inheritable | data[i].Permitted) & keep[i]
		} else {
			data[i].Effective &= keep[i]
			data[i].Inheritable &= keep[i]
		}
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("guest launcher: capset: %w", err)
	}
	// Read the sets back and fail closed: no_new_privs caps the payload's permitted set at this one
	// across the exec, also for a root payload whose bounding set could not shrink (issue #1496).
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("guest launcher: capget: %w", err)
	}
	for i := range data {
		if extra := (data[i].Permitted | data[i].Effective) &^ keep[i]; extra != 0 {
			return fmt.Errorf("guest launcher: capabilities outside the allowlist remain after capset (word %d, mask %#x)", i, extra)
		}
	}
	if !nonRoot {
		log.Info("guest launcher: the payload runs as root with the allowlist only (accepted on the vm tier, issue #1496)", "allow", allow)
	}
	if u.Mode != ModeOff && nonRoot {
		var failed []string
		for c := 0; c <= lastCap(); c++ {
			if keep[c/32]&(1<<(uint(c)%32)) == 0 {
				continue
			}
			if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, uintptr(c), 0, 0); err != nil {
				failed = append(failed, fmt.Sprintf("%d: %v", c, err))
			}
		}
		if len(failed) > 0 {
			if u.Mode == ModeRequire {
				return fmt.Errorf("guest launcher: ambient capability raise failed for the non-root user: %s", strings.Join(failed, "; "))
			}
			log.Warn("guest launcher: allowlisted capabilities could not be raised as ambient; the non-root payload will not hold them",
				"failed", failed)
		}
	}
	if !boundingDropped {
		log.Warn("guest launcher: could not shrink the bounding set (no CAP_SETPCAP); permitted, effective and inheritable are reduced to the allowlist",
			"allow", allow)
	} else {
		log.Info("guest launcher: capabilities reduced to the allowlist", "allow", allow)
	}
	return nil
}

// landlockABI returns the kernel's Landlock ABI version, 0 when Landlock is unavailable.
func landlockABI() int {
	const versionFlag = 1 << 0 // LANDLOCK_CREATE_RULESET_VERSION
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, versionFlag)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// minLandlockABI is the Landlock ABI the policy needs: V5 adds ioctl on device files, which the
// device rules grant on single nodes (ADR 0120 section 4.3). Below it a require policy refuses to
// start, and a best-effort one runs without Landlock rules.
const minLandlockABI = 5

// scopedLandlockABI is the Landlock ABI that adds the IPC scopes (LANDLOCK_SCOPE_SIGNAL, Linux 6.12).
const scopedLandlockABI = 6

func applyLandlock(l LandlockPolicy, log *slog.Logger) error {
	if l.Mode == ModeOff {
		log.Info("guest launcher: landlock off by policy")
		return nil
	}
	abi := landlockABI()
	if abi < minLandlockABI {
		if l.Mode == ModeRequire {
			if abi == 0 {
				return errors.New("guest launcher: landlock required but the kernel does not provide it")
			}
			return fmt.Errorf("guest launcher: landlock required but the kernel ABI is %d, version %d is needed", abi, minLandlockABI)
		}
		log.Warn("guest launcher: landlock unavailable or too old in this kernel, filesystem rules NOT applied; the gVisor tiers have a writable root by decision (issue #1150), so this gap is open and is recorded here", "kernel_abi", abi)
		return nil
	}
	scopeSignal := slices.Contains(l.Scoped, ScopeSignal)
	if scopeSignal && abi < scopedLandlockABI && l.Mode == ModeRequire {
		return fmt.Errorf("guest launcher: landlock signal scope required but the kernel ABI is %d, version %d is needed", abi, scopedLandlockABI)
	}
	var rules []landlock.Rule
	// Hidden paths (issue #1516) are left out of the read rules: the siblings are granted one by one.
	readExec, err := l.ReadExecVisible()
	if err != nil {
		return err
	}
	var readDirs, readFiles []string
	for _, e := range readExec {
		if st, serr := os.Stat(e); serr == nil && !st.IsDir() {
			readFiles = append(readFiles, e)
		} else {
			readDirs = append(readDirs, e)
		}
	}
	if len(readDirs) > 0 {
		rules = append(rules, landlock.RODirs(readDirs...).IgnoreIfMissing())
	}
	if len(readFiles) > 0 {
		rules = append(rules, landlock.ROFiles(readFiles...).IgnoreIfMissing())
	}
	if len(l.ReadWrite) > 0 {
		rules = append(rules, landlock.RWDirs(l.ReadWrite...).IgnoreIfMissing())
	}
	if len(l.ListDirs) > 0 {
		rules = append(rules, landlock.PathAccess(landlock.AccessFSSet(llsyscall.AccessFSReadDir), l.ListDirs...).IgnoreIfMissing())
	}
	// A device node is a file and gets the file rights; a directory such as /dev/pts gets the
	// directory rights too. Both allow ioctl on device files.
	var devFiles, devDirs []string
	for _, d := range l.Devices {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			devDirs = append(devDirs, d)
		} else {
			devFiles = append(devFiles, d)
		}
	}
	if len(devFiles) > 0 {
		rules = append(rules, landlock.RWFiles(devFiles...).WithIoctlDev().IgnoreIfMissing())
	}
	if len(devDirs) > 0 {
		rules = append(rules, landlock.RWDirs(devDirs...).WithIoctlDev().IgnoreIfMissing())
	}
	// Filesystem rules, and the signal scope when the policy names it. No network rules, and no
	// abstract Unix socket scope: they would change how agents talk to their tools.
	cfg := landlock.V5
	if scopeSignal {
		cfg = landlock.Config{HandledAccessFS: landlock.V5.HandledAccessFS, Scoped: llsyscall.ScopeSignal}
	}
	if l.Mode == ModeBestEffort {
		cfg = cfg.BestEffort()
	}
	restrict := cfg.RestrictPaths
	if scopeSignal {
		restrict = cfg.Restrict
	}
	if err := restrict(rules...); err != nil {
		if l.Mode == ModeRequire {
			return fmt.Errorf("guest launcher: landlock: %w", err)
		}
		log.Warn("guest launcher: landlock could not be applied, filesystem rules NOT applied; the gVisor tiers have a writable root by decision (issue #1150), so this gap is open and is recorded here", "error", err.Error())
		return nil
	}
	if scopeSignal && abi < scopedLandlockABI {
		// Only best_effort reaches this line: require stopped above.
		log.Warn("guest launcher: landlock signal scope NOT applied, the kernel ABI is too old; the payload can signal processes of its own uid", "kernel_abi", abi)
		scopeSignal = false
	}
	log.Info("guest launcher: landlock applied", "kernel_abi", abi, "read_exec", readExec, "hidden", l.Hidden, "read_write", l.ReadWrite, "devices", l.Devices, "scope_signal", scopeSignal)
	return nil
}

// applySeccomp loads the deny filter. Validate refuses any mode but require, and this check holds
// for a Policy built in code that skipped Validate: the vsock and process-access denies are never
// optional (ADR 0120 fail closed).
func applySeccomp(s SeccompPolicy, log *slog.Logger) error {
	if s.Mode != ModeRequire {
		return fmt.Errorf("guest launcher: seccomp mode %q is refused: it must be require", s.Mode)
	}
	insts, skipped, err := BuildSeccomp(s)
	if err == nil {
		err = loadSeccomp(insts)
	}
	if err != nil {
		return fmt.Errorf("guest launcher: seccomp: %w", err)
	}
	log.Info("guest launcher: seccomp filter applied", "denied", len(s.denyList())-len(skipped), "skipped_unknown", skipped)
	return nil
}

func loadSeccomp(insts []bpf.Instruction) error {
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return err
	}
	prog := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		prog[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	n := len(prog)
	if n == 0 || n > math.MaxUint16 {
		return fmt.Errorf("seccomp program has %d instructions, want 1..%d", n, math.MaxUint16)
	}
	fprog := unix.SockFprog{Len: uint16(n), Filter: &prog[0]}
	//nolint:gosec // seccomp(2) takes a pointer to the sock_fprog; no safe wrapper exists
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return errno
	}
	return nil
}
