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
	report(name, err == nil || !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES), detail+" "+fmt.Sprint(err))
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

// inheritedFd is where "inherit" leaves a socket without CLOEXEC, as a guest server could leave its
// vsock socket: the launcher must not pass it to the payload.
const inheritedFd = 7

// inherit plays the guest server before the launcher: it leaves a Unix socket at fd 7 without
// CLOEXEC, starts an outside process (no launcher, so no Landlock domain and no filter) that holds a
// socket at fd 3, and execs argv (the launcher, or the probe for the control run). The probe that
// runs next reads the pid from KOINON_PROBE_OTHER_PID.
func inherit(argv []string) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		err = unix.Dup3(fd, inheritedFd, 0)
	}
	if err != nil {
		fmt.Println("probe: inherit:", err)
		os.Exit(2)
	}
	holder := exec.Command(os.Args[0], "sleep") //nolint:gosec,noctx // the probe starts itself; it has no context
	holder.ExtraFiles = []*os.File{os.NewFile(uintptr(fd), "socket")}
	if err := holder.Start(); err != nil {
		fmt.Println("probe: cannot start the outside process:", err)
		os.Exit(2)
	}
	env := append(os.Environ(), fmt.Sprintf("KOINON_PROBE_OTHER_PID=%d", holder.Process.Pid))
	err = unix.Exec(argv[0], argv, env)
	fmt.Println("probe: exec:", err)
	os.Exit(2)
}

// blockedAny reports an attempt as blocked on any error, with the error as the detail.
func blockedAny(name string, err error, detail string) {
	report(name, err == nil, detail+" "+fmt.Sprint(err))
}

// fdChecks are the ways a payload could get a socket it did not create (docs/dev/guest-launcher.md,
// "The guest API channel").
func fdChecks() {
	_, err := unix.FcntlInt(inheritedFd, unix.F_GETFD, 0)
	blockedAny("inherited_fd", err, "fd 7 left open without CLOEXEC before the launcher")

	// Reopening a socket through its /proc fd link: the kernel refuses this for every socket (ENXIO).
	if s, serr := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0); serr == nil {
		f, oerr := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", s), os.O_RDWR, 0)
		if oerr == nil {
			_ = f.Close()
		}
		blockedAny("proc_reopen_own_socket", oerr, "open(/proc/self/fd/<own socket>)")
		_ = unix.Close(s)
	}

	other := os.Getenv("KOINON_PROBE_OTHER_PID")
	if other == "" {
		return
	}
	var pid int
	_, _ = fmt.Sscan(other, &pid)
	_, err = os.ReadDir("/proc/" + other + "/fd")
	blockedAny("proc_fd_list_other", err, "readdir(/proc/<outside pid>/fd)")
	f, err := os.OpenFile("/proc/"+other+"/fd/3", os.O_RDWR, 0) //nolint:gosec // the pid of the outside process the test started
	if err == nil {
		_ = f.Close()
	}
	blockedAny("proc_fd_open_other", err, "open(/proc/<outside pid>/fd/3), a socket")
	_, err = os.ReadFile("/proc/" + other + "/environ") //nolint:gosec // the pid of the outside process the test started
	blockedAny("proc_environ_other", err, "read(/proc/<outside pid>/environ)")

	buf := make([]byte, 8)
	local := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remote := []unix.RemoteIovec{{Base: 0x400000, Len: len(buf)}}
	_, err = unix.ProcessVMReadv(pid, local, remote, 0)
	denied("process_vm_readv_other", err, "process_vm_readv(<outside pid>)")
	err = unix.PtraceAttach(pid)
	denied("ptrace_other", err, "PTRACE_ATTACH <outside pid>")
	// Signal 0 runs the kernel's full permission check and delivers nothing (issue #1496: a root
	// payload must not signal the guest front, which runs as the same uid).
	denied("kill_other", unix.Kill(pid, 0), "kill(<outside pid>, 0)")
	pfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		fmt.Println("pidfd_getfd_other skip pidfd_open:", err)
		return
	}
	denied("pidfd_signal_other", unix.PidfdSendSignal(pfd, 0, nil, 0), "pidfd_send_signal(<outside pid>, 0)")
	got, err := unix.PidfdGetfd(pfd, 3, 0)
	if err == nil {
		_ = unix.Close(got)
	}
	denied("pidfd_getfd_other", err, "pidfd_getfd(<outside pid>, 3)")
	_ = unix.Close(pfd)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		time.Sleep(time.Minute)
		return
	}
	if len(os.Args) > 3 && os.Args[1] == "inherit" && os.Args[2] == "--" {
		inherit(os.Args[3:])
	}
	fdChecks()
	// ptrace of our own child: allowed to an unprivileged process, so only seccomp stops it.
	child := exec.Command(os.Args[0], "sleep") //nolint:gosec,noctx // the probe starts itself; it has no context
	if err := child.Start(); err != nil {
		fmt.Println("probe: cannot start child:", err)
		os.Exit(2)
	}
	runtime.LockOSThread()
	err := unix.PtraceAttach(child.Process.Pid)
	report("ptrace", err == nil, fmt.Sprint(err))
	// The payload still signals its own processes: agents stop the tools they start.
	err = child.Process.Kill()
	report("kill_own_child", err == nil, fmt.Sprint(err))

	empty, _ := unix.BytePtrFromString("")
	tmpfs, _ := unix.BytePtrFromString("tmpfs")
	dir, _ := unix.BytePtrFromString("/tmp")
	sys("mount", unix.SYS_MOUNT, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(dir)), uintptr(unsafe.Pointer(tmpfs)), 0, 0) //nolint:gosec // mount(2) takes C strings
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
	// Issue #1496: the capabilities a root payload must not hold. Each call has arguments that make
	// the kernel answer EBADF or EINVAL when nothing stops it, never a real effect.
	denied("finit_module", sysErrno(unix.SYS_FINIT_MODULE, ^uintptr(0), 0, 0), "finit_module(-1)")
	denied("reboot", sysErrno(unix.SYS_REBOOT, 0, 0, 0, 0), "reboot with no magic")
	rfd, rerr := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP)
	if rerr == nil {
		_ = unix.Close(rfd)
	}
	denied("socket_raw", rerr, "socket(AF_INET, SOCK_RAW)")

	// Issue #1503: the guest front, as root, writes its job logs in a directory under /run. The
	// payload must not create a file or a symlink there, or the front could follow the link.
	wf, werr := os.OpenFile("/run/koinon-probe", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if werr == nil {
		_ = wf.Close()
		_ = os.Remove("/run/koinon-probe")
	}
	denied("write:/run/koinon-probe", werr, "create a file under /run")
	lerr := os.Symlink("/etc/koinon-probe", "/run/koinon-link")
	if lerr == nil {
		_ = os.Remove("/run/koinon-link")
	}
	denied("symlink:/run/koinon-link", lerr, "plant a symlink under /run")
	// Issue #1516: nor may it read the job logs (the policy lists /run/koinon as hidden).
	_, rderr := os.ReadDir("/run/koinon/ate-jobs")
	denied("read:/run/koinon/ate-jobs", rderr, "list the job log directory")
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
	merr := unix.Mknod("/dev/koinon-blk", unix.S_IFBLK|0o600, int(unix.Mkdev(7, 0))) //nolint:gosec // loop device 7:0 fits in an int
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
