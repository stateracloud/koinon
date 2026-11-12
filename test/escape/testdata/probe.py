# SPDX-License-Identifier: Apache-2.0
"""Escape probe, run inside the sandbox as `python3 -` with this script on stdin.

The suite prepends CHECKS, a list of checks; the probe prints one JSON object mapping each
check's id to its result. Results name what happened (an errno name such as EACCES, "open",
"timeout") and never decide pass or fail: the Go specs do. Secret values arrive hex-encoded in
the script, so they are in no file, command line or environment the scan could find.
"""

import ctypes
import errno
import json
import os
import platform
import socket
import sys


def err_name(e):
    if isinstance(e, socket.timeout):
        return "timeout"
    if isinstance(e, socket.gaierror):
        return "gaierror"
    if e.errno is not None:
        return errno.errorcode.get(e.errno, str(e.errno))
    return type(e).__name__


def tcp(c):
    try:
        infos = socket.getaddrinfo(c["host"], c["port"], type=socket.SOCK_STREAM)
    except OSError as e:
        return err_name(e)
    family, kind, proto, _, addr = infos[0]
    s = socket.socket(family, kind, proto)
    s.settimeout(5)
    try:
        s.connect(addr)
        return "open"
    except OSError as e:
        return err_name(e)
    finally:
        s.close()


def udp_dns(c):
    # A real query for example.com; an answer means UDP reached a resolver.
    q = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
    family = socket.AF_INET6 if ":" in c["host"] else socket.AF_INET
    s = socket.socket(family, socket.SOCK_DGRAM)
    s.settimeout(5)
    try:
        s.sendto(q, (c["host"], 53))
        s.recvfrom(512)
        return "answered"
    except OSError as e:
        return err_name(e)
    finally:
        s.close()


def resolve(c):
    try:
        return sorted({i[4][0] for i in socket.getaddrinfo(c["name"], 443, type=socket.SOCK_STREAM)})
    except OSError as e:
        return err_name(e)


def read(c):
    """Reads a file or lists a directory: "ok", or the errno name."""
    try:
        if os.path.isdir(c["path"]):
            os.listdir(c["path"])
        else:
            with open(c["path"], "rb") as f:
                f.read(1)
        return "ok"
    except OSError as e:
        return err_name(e)


def env(_):
    return dict(os.environ)


SKIP = ("/proc", "/sys", "/dev")


def scan(c):
    """Paths whose content holds any of the needles: every readable file under the roots
    (Landlock hides "/" itself, so the suite names them) outside /proc, /sys and /dev, plus the
    environment and command line of every other process."""
    needles = [bytes.fromhex(n) for n in c["needles"]]
    found, files = [], 0

    def check(path):
        nonlocal files
        try:
            with open(path, "rb") as f:
                data = f.read(64 << 20)
        except OSError:
            return
        files += 1
        if any(n in data for n in needles):
            found.append(path)

    seen = set()
    for top in c["roots"]:
        for root, dirs, names in os.walk(top, followlinks=False):
            dirs[:] = [d for d in dirs if os.path.join(root, d) not in SKIP]
            for name in names:
                path = os.path.join(root, name)
                if path not in seen and os.path.isfile(path) and not os.path.islink(path):
                    seen.add(path)
                    check(path)
    me = str(os.getpid())
    for pid in os.listdir("/proc"):
        if pid.isdigit() and pid != me:
            for part in ("environ", "cmdline"):
                check(f"/proc/{pid}/{part}")
    return {"found": found, "files": files}


SKIP_FIND = ("/proc", "/sys", "/dev")


def find(c):
    """Paths under the roots (outside /proc, /sys and /dev) with any of the substrings in them,
    and how many entries were visited (a walk that cannot read a directory skips it silently)."""
    found, visited = [], 0
    segs = set(c.get("segments", ()))  # whole path components, so "aspire" does not match "spire"
    for top in c["roots"]:
        if top in SKIP_FIND:
            continue
        for root, dirs, names in os.walk(top, followlinks=False):
            dirs[:] = [d for d in dirs if os.path.join(root, d) not in SKIP_FIND]
            for name in dirs + names:
                visited += 1
                path = os.path.join(root, name)
                if any(s in path for s in c.get("contains", ())) or (
                        segs and (name in segs or any(p in segs for p in path.split(os.sep)))):
                    found.append(path)
    return {"found": sorted(found), "visited": visited}


def mounts(c):
    """Mount table lines that mention any of the substrings."""
    with open("/proc/self/mountinfo") as f:
        return [l.strip() for l in f if any(s in l for s in c["contains"])]


def status(_):
    """The security fields of /proc/self/status, as strings."""
    with open("/proc/self/status") as f:
        fields = dict(l.split(":", 1) for l in f if ":" in l)
    return {k: fields[k].strip() for k in ("CapEff", "CapPrm", "CapBnd", "NoNewPrivs", "Seccomp") if k in fields}


def write(c):
    """Creates (and removes) a file: "ok", or the errno name."""
    try:
        with open(c["path"], "w") as f:
            f.write("x")
        os.unlink(c["path"])
        return "ok"
    except OSError as e:
        return err_name(e)


def mode(c):
    """The permission bits of a path as an octal string (sticky included), or the errno name."""
    try:
        return format(os.stat(c["path"]).st_mode & 0o7777, "o")
    except OSError as e:
        return err_name(e)


libc = ctypes.CDLL(None, use_errno=True)
# System call numbers by machine: bpf, keyctl, add_key, perf_event_open.
NR = {
    "x86_64": {"bpf": 321, "keyctl": 250, "add_key": 248, "perf_event_open": 298},
    "aarch64": {"bpf": 280, "keyctl": 219, "add_key": 217, "perf_event_open": 241},
}.get(platform.machine(), {})


def sysresult(rc):
    return "ok" if rc >= 0 else errno.errorcode.get(ctypes.get_errno(), str(ctypes.get_errno()))


def syscall(c):
    """A privileged or filtered operation, attempted for real: "ok" or the errno name."""
    n = c["name"]
    if n == "mount":
        return sysresult(libc.mount(b"none", b"/tmp", b"tmpfs", 0, None))
    if n == "unshare_user":
        return sysresult(libc.unshare(0x10000000))
    if n == "unshare_mount":
        return sysresult(libc.unshare(0x00020000))
    if n == "bpf":
        return sysresult(libc.syscall(NR["bpf"], 5, None, 0))
    if n == "keyctl":
        return sysresult(libc.syscall(NR["keyctl"], 0, -3, 0))
    if n == "add_key":
        return sysresult(libc.syscall(NR["add_key"], b"user", b"k", b"v", 1, -2))
    if n == "perf_event_open":
        return sysresult(libc.syscall(NR["perf_event_open"], None, 0, -1, -1, 0))
    if n == "setuid0":
        return sysresult(libc.setuid(0))
    if n == "ptrace_pid1":
        return sysresult(libc.ptrace(16, 1, None, None))  # PTRACE_ATTACH
    if n == "proc1_mem":
        try:
            with open("/proc/1/mem", "rb") as f:
                f.read(1)
            return "ok"
        except OSError as e:
            return err_name(e)
    if n == "raw_packet":
        return rawsock(socket.AF_PACKET)
    if n == "raw_inet":
        return rawsock(socket.AF_INET)
    raise ValueError(n)


def rawsock(family):
    try:
        socket.socket(family, socket.SOCK_RAW, 0).close()
        return "ok"
    except OSError as e:
        return err_name(e)


def kernel(_):
    with open("/proc/cpuinfo") as f:
        hypervisor = any(l.startswith("flags") and " hypervisor" in l for l in f)
    return {"release": os.uname().release, "hypervisor": hypervisor}


OPS = {"tcp": tcp, "udp_dns": udp_dns, "resolve": resolve, "read": read, "env": env, "scan": scan,
       "find": find, "mounts": mounts, "status": status, "write": write, "mode": mode, "syscall": syscall, "kernel": kernel}
json.dump({c["id"]: OPS[c["op"]](c) for c in CHECKS}, sys.stdout)  # noqa: F821
