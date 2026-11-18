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
	"fmt"
	"io"
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
	// Devices are read-write paths that also allow ioctl on device files (/dev).
	Devices []string `json:"devices"`
}

// SeccompPolicy is the deny filter: every other syscall stays allowed.
type SeccompPolicy struct {
	Mode Mode `json:"mode"`
	// Deny replaces the built-in deny list when non-empty; DenyExtra adds to whichever list applies.
	Deny      []string `json:"deny"`
	DenyExtra []string `json:"deny_extra"`
}

// UserPolicy drops the payload to a non-root user.
//
// Phase 0 (issue #1087 comment, Substrate v1.0.0 microVM) found that the guest gives payloads no
// CAP_SETUID or CAP_SETGID, so the drop fails there. ModeBestEffort (the default) logs that and
// continues as the current user with every other control applied; ModeRequire refuses to start.
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
	"kexec_load", "kexec_file_load", "init_module", "finit_module", "delete_module",
	"create_module", "get_kernel_syms", "query_module",
	// host state
	"reboot", "swapon", "swapoff", "acct", "settimeofday", "clock_settime", "clock_adjtime",
	"adjtimex", "ioperm", "iopl", "quotactl", "syslog", "vhangup", "lookup_dcookie",
	"nfsservctl", "_sysctl", "uselib", "open_by_handle_at", "name_to_handle_at",
	"fanotify_init", "move_pages", "mbind", "set_mempolicy", "migrate_pages",
}

// DefaultPolicy is the policy used when no file is given.
func DefaultPolicy() Policy {
	return Policy{
		Landlock: LandlockPolicy{
			Mode:      ModeBestEffort,
			ReadExec:  []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc", "/opt", "/proc", "/sys", "/run", "/var/lib"},
			ReadWrite: []string{"/sandbox", "/tmp", "/var/tmp", "/home", "/root", "/var/cache", "/var/log"},
			Devices:   []string{"/dev"},
		},
		Seccomp: SeccompPolicy{Mode: ModeRequire},
		User:    UserPolicy{Mode: ModeBestEffort},
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
		p.Landlock.ReadExec, p.Landlock.ReadWrite, p.Landlock.Devices = nil, nil, nil
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
	for _, c := range p.Capabilities.Allow {
		if _, ok := capNumber(c); !ok {
			return fmt.Errorf("guest launcher policy: unknown capability %q", c)
		}
	}
	for _, list := range [][]string{p.Landlock.ReadExec, p.Landlock.ReadWrite, p.Landlock.Devices} {
		for _, path := range list {
			if !strings.HasPrefix(path, "/") {
				return fmt.Errorf("guest launcher policy: landlock path %q is not absolute", path)
			}
		}
	}
	if p.User.Mode == ModeRequire && p.User.UID == 0 {
		return fmt.Errorf("guest launcher policy: user mode require needs a non-zero uid")
	}
	return nil
}
