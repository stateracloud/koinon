// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Command probe is the escape probe the guest launcher test runs inside a container, ported from
// hack/spike/probe/escape_probe.py (items 8.9, 8.11 and 8.12) as a static Go program so the test
// image needs no Python. Each attempt prints "name open|blocked detail".
package main

import (
	"errors"
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

// denied reports an attempt that failed for want of permission: EPERM from seccomp or a capability,
// EACCES from Landlock. Any other result, success included, leaves the attempt open.
func denied(name string, err error, detail string) {
	report(name, err == nil || !(errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)), detail+" "+fmt.Sprint(err))
}

// sysErrno runs a raw syscall and returns its errno as an error (nil on success).
func sysErrno(nr uintptr, a ...uintptr) error {
	var args [6]uintptr
	copy(args[:], a)
	if _, _, e := unix.Syscall6(nr, args[0], args[1], args[2], args[3], args[4], args[5]); e != 0 {
		return e
	}
	return nil
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

	// Calls whose arguments make the kernel answer EINVAL or EFAULT when nothing filters them, so a
	// seccomp answer is told apart from the kernel's own.
	denied("clone_newuser", sysErrno(unix.SYS_CLONE, unix.CLONE_NEWUSER|unix.CLONE_THREAD, 0, 0, 0, 0), "clone(CLONE_NEWUSER|CLONE_THREAD)")
	err = sysErrno(unix.SYS_CLONE3, 0, 0)
	report("clone3", !errors.Is(err, unix.ENOSYS), "ENOSYS expected under the launcher: "+fmt.Sprint(err))
	denied("io_uring_setup", sysErrno(unix.SYS_IO_URING_SETUP, 0, 0), "io_uring_setup(0, NULL)")
	fd, serr := unix.Socket(40, unix.SOCK_STREAM, 0) // AF_VSOCK
	if serr == nil {
		_ = unix.Close(fd)
	}
	denied("socket_vsock", serr, "socket(AF_VSOCK)")

	// Devices: the listed nodes work, nothing else under /dev does.
	f, ferr := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if ferr == nil {
		_ = f.Close()
	}
	report("dev_allowed:/dev/null", ferr == nil, fmt.Sprint(ferr))
	listed, derr := os.ReadDir("/dev")
	report("dev_list", derr == nil && len(listed) > 0, fmt.Sprint(derr))
	f, ferr = os.OpenFile("/dev/koinon-probe", os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr == nil {
		_ = f.Close()
		_ = os.Remove("/dev/koinon-probe")
	}
	denied("dev_create:/dev/koinon-probe", ferr, "create a regular file in /dev")
	merr := unix.Mknod("/dev/koinon-blk", unix.S_IFBLK|0o600, int(unix.Mkdev(7, 0)))
	if merr == nil {
		_ = os.Remove("/dev/koinon-blk")
	}
	denied("dev_mknod_block", merr, "mknod a block device in /dev")
	f, ferr = os.OpenFile("/dev/koinon-unlisted", os.O_WRONLY, 0)
	if ferr == nil {
		_ = f.Close()
	}
	denied("dev_open_unlisted:/dev/koinon-unlisted", ferr, "a node /dev does not name")
	blockDev := ""
	if entries, derr := os.ReadDir("/dev"); derr == nil {
		for _, e := range entries {
			if info, ierr := e.Info(); ierr == nil && info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
				blockDev = "/dev/" + e.Name()
				break
			}
		}
	}
	if blockDev == "" {
		fmt.Println("dev_write_blockdev skip no block device node in /dev")
	} else {
		f, ferr = os.OpenFile(blockDev, os.O_WRONLY, 0)
		if ferr == nil {
			_ = f.Close()
		}
		denied("dev_write_blockdev", ferr, blockDev)
	}

	for _, p := range []string{"/etc/koinon-probe", "/usr/koinon-probe", "/var/koinon-probe", "/sandbox/koinon-probe", "/tmp/koinon-probe"} {
		write(p)
	}
	_, err = os.ReadFile("/proc/self/status")
	report("read:/proc/self/status", err == nil, fmt.Sprint(err))

	st, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(l, "CapEff:") || strings.HasPrefix(l, "CapBnd:") || strings.HasPrefix(l, "CapPrm:") || strings.HasPrefix(l, "CapAmb:") || strings.HasPrefix(l, "NoNewPrivs:") {
			fmt.Println("status", strings.Join(strings.Fields(l), " "))
		}
	}
	fmt.Println("uid", os.Geteuid())
}
