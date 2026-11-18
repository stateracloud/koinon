// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Command probe is the escape probe the guest launcher test runs inside a container, ported from
// hack/spike/probe/escape_probe.py (items 8.9, 8.11 and 8.12) as a static Go program so the test
// image needs no Python. Each attempt prints "name open|blocked detail".
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func report(name string, open bool, detail string) {
	v := "blocked"
	if open {
		v = "open"
	}
	fmt.Println(name, v, detail)
}

func sys(name string, nr uintptr, a ...uintptr) {
	var args [6]uintptr
	copy(args[:], a)
	_, _, e := unix.Syscall6(nr, args[0], args[1], args[2], args[3], args[4], args[5])
	report(name, e == 0, fmt.Sprint(e))
}

func write(path string) {
	err := os.WriteFile(path, []byte("x"), 0o600)
	if err == nil {
		_ = os.Remove(path)
	}
	report("write:"+path, err == nil, fmt.Sprint(err))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		time.Sleep(time.Minute)
		return
	}
	// ptrace of our own child: allowed to an unprivileged process, so only seccomp stops it.
	child := exec.Command(os.Args[0], "sleep")
	if err := child.Start(); err != nil {
		fmt.Println("probe: cannot start child:", err)
		os.Exit(2)
	}
	runtime.LockOSThread()
	err := unix.PtraceAttach(child.Process.Pid)
	report("ptrace", err == nil, fmt.Sprint(err))
	_ = child.Process.Kill()

	empty, _ := unix.BytePtrFromString("")
	tmpfs, _ := unix.BytePtrFromString("tmpfs")
	dir, _ := unix.BytePtrFromString("/tmp")
	sys("mount", unix.SYS_MOUNT, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(dir)), uintptr(unsafe.Pointer(tmpfs)), 0, 0)
	sys("bpf", unix.SYS_BPF, 5, 0, 0)
	sys("keyctl", unix.SYS_KEYCTL, 0, ^uintptr(2), 0) // KEYCTL_GET_KEYRING_ID, KEY_SPEC_USER_SESSION_KEYRING (-3)
	sys("perf_event_open", unix.SYS_PERF_EVENT_OPEN, 0, 0, ^uintptr(0), ^uintptr(0), 0)
	sys("unshare_newuser", unix.SYS_UNSHARE, unix.CLONE_NEWUSER)
	sys("unshare_newns", unix.SYS_UNSHARE, unix.CLONE_NEWNS)

	for _, p := range []string{"/etc/koinon-probe", "/usr/koinon-probe", "/var/koinon-probe", "/sandbox/koinon-probe", "/tmp/koinon-probe"} {
		write(p)
	}
	_, err = os.ReadFile("/proc/self/status")
	report("read:/proc/self/status", err == nil, fmt.Sprint(err))

	st, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(l, "CapEff:") || strings.HasPrefix(l, "NoNewPrivs:") {
			fmt.Println("status", strings.Join(strings.Fields(l), " "))
		}
	}
	fmt.Println("uid", os.Geteuid())
}
