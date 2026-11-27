// SPDX-License-Identifier: Apache-2.0

// Package guestlaunch is the hardening step of the Koinon guest launcher (ADR 0119, issue #1087).
// The launcher runs inside a Substrate guest (a microVM or gVisor), applies no_new_privs, a
// capability allowlist, Landlock filesystem rules and a seccomp deny filter to itself, and then
// execs the payload (the agent). The policy is a JSON file that the flavor image carries; it holds
// no credentials. Without a file the built-in DefaultPolicy applies.
package guestlaunch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// Mode says what the launcher does when a control is unavailable in the guest.
type Mode string

const (
	// ModeRequire fails closed: the payload does not start without the control.
	ModeRequire Mode = "require"
	// ModeBestEffort applies the control when the kernel has it and logs one clear line when not.
	ModeBestEffort Mode = "best_effort"
	// ModeOff skips the control.
	ModeOff Mode = "off"
)

// Policy is the launcher configuration.
type Policy struct {
	// Capabilities keeps only the listed capabilities (names such as CAP_KILL); all others are
	// dropped from the bounding, permitted, effective, inheritable and ambient sets.
	Capabilities CapabilityPolicy `json:"capabilities"`
	Landlock     LandlockPolicy   `json:"landlock"`
	Seccomp      SeccompPolicy    `json:"seccomp"`
	User         UserPolicy       `json:"user"`
}

// CapabilityPolicy is the capability allowlist. An empty list drops every capability.
type CapabilityPolicy struct {
	Allow []string `json:"allow"`
}

// LandlockPolicy lists the filesystem rules. A path in no list is not accessible at all.
type LandlockPolicy struct {
	Mode Mode `json:"mode"`
	// ReadExec paths may be read and executed.
	ReadExec []string `json:"read_exec"`
	// ReadWrite paths may be read, written, created and removed (execution is allowed too: agents
	// run what they install under their home).
	ReadWrite []string `json:"read_write"`
	// Devices are the device nodes (files) and device directories that may be read, written and
	// used with ioctl, each named one by one (ADR 0120 section 4.3: null, zero, full, random,
	// urandom, tty, ptmx, pts, shm). Nothing else under /dev is accessible.
	Devices []string `json:"devices"`
	// ListDirs are directories whose entries may be listed and nothing else: /dev is listed so
	// tools work, but a node in it that Devices does not name cannot be opened or created.
	ListDirs []string `json:"list_dirs"`
	// Hidden are paths below a ReadExec entry that the payload may not read, list or execute, even as
	// root (issue #1516: the job logs of the guest front under /run/koinon). Landlock only adds rights,
	// so the launcher grants the siblings of a hidden path and leaves the path out (ReadExecVisible).
	Hidden []string `json:"hidden,omitempty"`
	// Scoped lists the Landlock IPC scopes (ABI 6). The one known scope is "signal": the payload
	// cannot send a signal to a process outside its Landlock domain, such as the guest front and
	// the other Koinon processes of the guest, even when it runs as the same uid (issue #1496). The
	// scope follows Mode: in require mode a kernel below ABI 6 stops the start.
	Scoped []string `json:"scoped,omitempty"`
}

// ScopeSignal is the Landlock scope that blocks signals to processes outside the payload's domain.
const ScopeSignal = "signal"

// RootAllowedCapabilities are the only capabilities that a payload which can run as root may keep
// (issue #1496). Any other capability could trace, signal or reconfigure the Koinon processes of
// the guest, create device nodes (CAP_MKNOD) or widen the guest kernel's attack surface. It is an
// allowlist, so a capability that a later kernel adds is refused by default. Validate refuses a
// policy whose allowlist holds any other capability unless the user drop is required.
var RootAllowedCapabilities = []string{"CAP_NET_BIND_SERVICE", "CAP_AUDIT_WRITE"}

// SeccompPolicy is the deny filter: every other syscall stays allowed.
type SeccompPolicy struct {
	Mode Mode `json:"mode"`
	// Deny replaces the built-in deny list when non-empty; DenyExtra adds to whichever list applies.
	Deny      []string `json:"deny"`
	DenyExtra []string `json:"deny_extra"`
}

// DefaultDevices are the device paths a payload may use. A block device or any other node under
// /dev is outside the list on purpose.
var DefaultDevices = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/ptmx",
	"/dev/pts", "/dev/shm",
}

// UserPolicy drops the payload to a non-root user.
//
// ModeRequire (the default, issue #1387) refuses to start the payload when the drop fails. The
// gVisor tiers give the agent container CAP_SETUID, CAP_SETGID and CAP_SETPCAP for the drop (the
// actor template, substrate.GVisorCapAdd). ModeBestEffort logs a failed drop and continues as the
// current user with every other control applied: only the vm tier keeps it, because phase 0 (issue
// #1087, Substrate v1.0.1 microVM) found no CAP_SETUID or CAP_SETGID there and the drop is not yet
// proven on it (issue #1462).
type UserPolicy struct {
	Mode Mode   `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
}

// DefaultSeccompDeny is the built-in deny list: syscalls an agent never needs and that widen the
// kernel attack surface or let a process get out of its confinement. It follows the reasoning of
// Docker's default profile (calls gated on a capability there are denied outright here, because
// the capability allowlist is empty by default). Names a CPU architecture lacks are skipped.
var DefaultSeccompDeny = []string{
	// inspect or control other processes
	"ptrace", "process_vm_readv", "process_vm_writev", "kcmp", "pidfd_getfd",
	// mounts and namespaces
	"mount", "umount", "umount2", "pivot_root", "move_mount", "open_tree", "fsopen", "fsconfig",
	"fsmount", "fspick", "mount_setattr", "setns", "unshare",
	// kernel code, keys and tracing
	"bpf", "keyctl", "add_key", "request_key", "perf_event_open", "userfaultfd",
	// io_uring runs file and network operations in the kernel outside the seccomp and Landlock
	// checks of the calls it replaces, and is a steady source of kernel bugs.
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	"kexec_load", "kexec_file_load", "init_module", "finit_module", "delete_module",
	"create_module", "get_kernel_syms", "query_module",
	// host state
	"reboot", "swapon", "swapoff", "acct", "settimeofday", "clock_settime", "clock_adjtime",
	"adjtimex", "ioperm", "iopl", "quotactl", "syslog", "vhangup", "lookup_dcookie",
	"nfsservctl", "_sysctl", "uselib", "open_by_handle_at", "name_to_handle_at",
	"fanotify_init", "move_pages", "mbind", "set_mempolicy", "migrate_pages",
}

// DefaultUID is the uid and the gid of the sandbox user that the payload runs as.
const DefaultUID = 10001

// DefaultPolicy is the policy used when no file is given.
func DefaultPolicy() Policy {
	return Policy{
		Landlock: LandlockPolicy{
			Mode:      ModeBestEffort,
			ReadExec:  []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc", "/opt", "/proc", "/sys", "/run", "/var/lib"},
			ReadWrite: []string{"/sandbox", "/tmp", "/var/tmp", "/home", "/root", "/var/cache", "/var/log"},
			Devices:   slices.Clone(DefaultDevices),
			ListDirs:  []string{"/dev"},
			Hidden:    []string{"/run/koinon"},
		},
		Seccomp: SeccompPolicy{Mode: ModeRequire},
		// Uid and gid 10001 are the sandbox user of the harness images (docs/dev/images.md).
		User: UserPolicy{Mode: ModeRequire, UID: DefaultUID, GID: DefaultUID},
	}
}

// ParsePolicy reads a JSON policy. Unknown fields are an error, a missing mode takes the default of
// its control, and the result is validated.
func ParsePolicy(r io.Reader) (Policy, error) {
	p := DefaultPolicy()
	// A file replaces the path lists as a whole rather than appending to the defaults.
	var raw struct {
		Landlock *struct{} `json:"landlock"`
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return Policy{}, err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Policy{}, fmt.Errorf("guest launcher policy: %w", err)
	}
	if raw.Landlock != nil {
		p.Landlock.ReadExec, p.Landlock.ReadWrite, p.Landlock.Devices, p.Landlock.ListDirs, p.Landlock.Hidden = nil, nil, nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("guest launcher policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Validate checks the policy before anything is applied, so a typo fails the start instead of
// silently weakening a control.
func (p Policy) Validate() error {
	for name, m := range map[string]Mode{"landlock": p.Landlock.Mode, "seccomp": p.Seccomp.Mode, "user": p.User.Mode} {
		switch m {
		case ModeRequire, ModeBestEffort, ModeOff:
		default:
			return fmt.Errorf("guest launcher policy: %s mode %q must be require, best_effort or off", name, m)
		}
	}
	// The filter carries the vsock deny and the process-access denies, so no policy may skip it
	// (ADR 0120 fail closed). Off and best_effort (which skips the whole filter when it fails to
	// load) are refused.
	if p.Seccomp.Mode != ModeRequire {
		return fmt.Errorf("guest launcher policy: seccomp mode %q is refused: it must be require", p.Seccomp.Mode)
	}
	for _, c := range p.Capabilities.Allow {
		n, ok := capNumber(c)
		if !ok {
			return fmt.Errorf("guest launcher policy: unknown capability %q", c)
		}
		// A policy without a required drop can leave the payload root (the vm tier, issue #1496).
		if p.User.Mode != ModeRequire {
			allowed := false
			for _, a := range RootAllowedCapabilities {
				if an, _ := capNumber(a); an == n {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("guest launcher policy: capability %s is refused for a payload that can run as root (user mode %q); allowed: %s", c, p.User.Mode, strings.Join(RootAllowedCapabilities, ", "))
			}
		}
	}
	for _, s := range p.Landlock.Scoped {
		if s != ScopeSignal {
			return fmt.Errorf("guest launcher policy: unknown landlock scope %q (known: %s)", s, ScopeSignal)
		}
	}
	for _, list := range [][]string{p.Landlock.ReadExec, p.Landlock.ReadWrite, p.Landlock.Devices, p.Landlock.ListDirs, p.Landlock.Hidden} {
		for _, path := range list {
			if !strings.HasPrefix(path, "/") {
				return fmt.Errorf("guest launcher policy: landlock path %q is not absolute", path)
			}
		}
	}
	for _, h := range p.Landlock.Hidden {
		if path.Clean(h) == "/" {
			return fmt.Errorf("guest launcher policy: landlock hidden path %q is the root directory", h)
		}
		// A read rule on or below a hidden path would grant it again.
		for _, e := range p.Landlock.ReadExec {
			if pathCovers(h, e) {
				return fmt.Errorf("guest launcher policy: landlock read_exec path %q is inside hidden path %q", e, h)
			}
		}
		// Rights on a writable parent cannot be taken back, and a hidden path must not be writable.
		for _, rw := range p.Landlock.ReadWrite {
			if pathCovers(rw, h) {
				return fmt.Errorf("guest launcher policy: landlock hidden path %q is inside read_write path %q", h, rw)
			}
		}
		for _, list := range [][]string{p.Landlock.Devices, p.Landlock.ListDirs} {
			for _, e := range list {
				if pathCovers(e, h) {
					return fmt.Errorf("guest launcher policy: landlock hidden path %q is inside path %q", h, e)
				}
			}
		}
	}
	// A name no architecture has is a typo, and a typo must not silently leave a call allowed. A name
	// only another architecture has is fine: BuildSeccomp skips it on this one.
	known := knownSyscalls()
	for _, list := range [][]string{p.Seccomp.Deny, p.Seccomp.DenyExtra} {
		for _, n := range list {
			if !known[n] {
				return fmt.Errorf("guest launcher policy: seccomp names unknown syscall %q", n)
			}
		}
	}
	if p.User.Mode == ModeRequire && p.User.UID == 0 {
		return fmt.Errorf("guest launcher policy: user mode require needs a non-zero uid")
	}
	if p.User.Mode == ModeRequire && p.User.GID == 0 {
		return fmt.Errorf("guest launcher policy: user mode require needs a non-zero gid")
	}
	return nil
}

// pathCovers says whether a rule on dir reaches p: dir itself, or anything below it.
func pathCovers(dir, p string) bool {
	dir, p = path.Clean(dir), path.Clean(p)
	return dir == p || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// ReadExecVisible is ReadExec without the Hidden paths. Landlock has no deny rule, so an entry that
// holds a hidden path is replaced by its children (and their children, down to the hidden path), read
// at the time of the call. The payload then reads nothing that the front or another service makes
// in a directory above a hidden path after this call; the front makes its own directory first
// (guestfront.PrepareLogDir) and the launcher runs after it. A directory that does not exist is
// skipped, as Landlock skips a missing path. Any other read error is returned: the caller must not
// start the payload with a wider rule than the policy says.
func (l LandlockPolicy) ReadExecVisible() ([]string, error) {
	var out []string
	for _, e := range l.ReadExec {
		got, err := without(path.Clean(e), l.Hidden)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// without returns dir, or the entries below it that are not hidden, when a hidden path is at or below dir.
func without(dir string, hidden []string) ([]string, error) {
	var below []string
	for _, h := range hidden {
		h = path.Clean(h)
		if h == dir {
			return nil, nil
		}
		if pathCovers(dir, h) {
			below = append(below, h)
		}
	}
	if len(below) == 0 {
		return []string{dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("guest launcher policy: list %s to hide %s: %w", dir, below[0], err)
	}
	var out []string
	for _, ent := range entries {
		// A symlink is skipped: Landlock follows it and would grant its target, which the policy never
		// granted (a read through the link is checked against the target).
		if ent.Type()&fs.ModeSymlink != 0 {
			continue
		}
		got, err := without(path.Join(dir, ent.Name()), below)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}
