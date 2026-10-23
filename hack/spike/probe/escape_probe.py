# SPDX-License-Identifier: Apache-2.0
"""Spike step 8 probe: escape attempts from inside a sandbox, as the agent user.

Every attempt prints BLOCKED or OPEN with the error. Targets come from the
command line as name=host:port[/udp] so the probe holds no cluster knowledge.
"""

import ctypes
import errno
import os
import socket
import subprocess
import sys

libc = ctypes.CDLL(None, use_errno=True)
results = []


def record(item, what, ok_means_open, err=""):
    verdict = "OPEN" if ok_means_open else "BLOCKED"
    results.append((item, what, verdict, err))
    print(f"{item:5} {verdict:7} {what} {err}".rstrip(), flush=True)


def tcp(item, name, host, port):
    try:
        s = socket.create_connection((host, port), timeout=4)
        s.close()
        record(item, f"tcp {name} {host}:{port}", True)
    except OSError as e:
        record(item, f"tcp {name} {host}:{port}", False, f"({e.__class__.__name__}: {e})")


def udp_dns(item, name, host):
    # A real DNS query; an answer means UDP reached a resolver.
    q = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.settimeout(4)
        s.sendto(q, (host, 53))
        data, _ = s.recvfrom(512)
        record(item, f"udp dns {name} {host}:53", True, f"({len(data)} bytes answered)")
    except OSError as e:
        record(item, f"udp dns {name} {host}:53", False, f"({e.__class__.__name__}: {e})")


def syscall(item, name, nr, *args):
    rc = libc.syscall(nr, *args)
    e = ctypes.get_errno()
    record(item, f"syscall {name}", rc >= 0, f"(rc={rc} errno={errno.errorcode.get(e, e)})")


def try_write(item, path):
    try:
        with open(path, "w") as f:
            f.write("x")
        record(item, f"write {path}", True)
        os.unlink(path)
    except OSError as e:
        record(item, f"write {path}", False, f"({e.strerror})")


def main():
    targets = [a.split("=", 1) for a in sys.argv[1:]]
    for spec in targets:
        item, rest = spec[0].split(":", 1)  # "8.1:public" -> item 8.1, name public
        item, name = spec[0].split(":", 1)[0], spec[0].split(":", 1)[1]
        hostport = spec[1]
        if hostport.endswith("/udp"):
            udp_dns(item, name, hostport[:-4].rsplit(":", 1)[0])
            continue
        host, port = hostport.rsplit(":", 1)
        tcp(item, name, host.strip("[]"), int(port))

    # 8.3: an arbitrary resolver by name lookup through the sandbox resolver
    try:
        addr = socket.gethostbyname("x" * 50 + ".exfil-probe.koinon.invalid")
        record("8.3", "resolve long random name via sandbox resolver", not addr.startswith("198.18."), f"(answer {addr}; 198.18.x is OpenShell's synthetic, no upstream query)")
    except OSError as e:
        record("8.3", "resolve long random name via sandbox resolver", False, f"({e})")

    # 8.7: ServiceAccount tokens
    for p in ["/var/run/secrets/kubernetes.io/serviceaccount/token", "/run/secrets/kubernetes.io/serviceaccount/token",
              "/var/run/secrets/tokens", "/.openshell/pod-identity"]:
        try:
            if os.path.isdir(p):
                os.listdir(p)
                record("8.7", f"list {p}", True)
            else:
                open(p).read(1)
                record("8.7", f"read {p}", True)
        except OSError as e:
            record("8.7", f"read {p}", False, f"({e.strerror})")

    # 8.9: ptrace attach to PID 1 (in-VM runtime) and read its memory
    PTRACE_ATTACH = 16
    rc = libc.ptrace(PTRACE_ATTACH, 1, None, None)
    record("8.9", "ptrace attach pid 1", rc == 0, f"(errno={errno.errorcode.get(ctypes.get_errno(), '?')})")
    try:
        with open("/proc/1/mem", "rb") as f:
            f.read(1)
        record("8.9", "read /proc/1/mem", True)
    except OSError as e:
        record("8.9", "read /proc/1/mem", False, f"({e.strerror})")

    # 8.10: VM and PID namespace
    with open("/proc/1/comm") as f:
        pid1 = f.read().strip()
    record("8.10", f"pid 1 is the host init (comm={pid1}, kernel={os.uname().release})", pid1 in ("systemd", "init", "machined"))

    # 8.11: mount, raw sockets, capabilities, privilege
    syscall("8.11", "mount tmpfs /tmp/m", 165, b"none", b"/tmp", b"tmpfs", 0, None)
    for fam, typ, label in [(socket.AF_PACKET, socket.SOCK_RAW, "AF_PACKET raw"), (socket.AF_INET, socket.SOCK_RAW, "AF_INET raw")]:
        try:
            socket.socket(fam, typ, 0).close()
            record("8.11", f"socket {label}", True)
        except OSError as e:
            record("8.11", f"socket {label}", False, f"({e.strerror})")
    with open("/proc/self/status") as f:
        status = dict(l.split(":", 1) for l in f if ":" in l)
    cap = int(status["CapEff"].strip(), 16) | int(status["CapPrm"].strip(), 16)
    record("8.11", f"capabilities CapEff/CapPrm={cap:#x} NoNewPrivs={status['NoNewPrivs'].strip()}", cap != 0)
    rc = libc.setuid(0)
    record("8.11", "setuid(0)", rc == 0, f"(errno={errno.errorcode.get(ctypes.get_errno(), '?')})")
    out = subprocess.run(["sh", "-c", "find / -xdev -perm -4000 -type f 2>/dev/null | head -5"], capture_output=True, text=True).stdout.split()
    record("8.11", f"setuid binaries reachable: {out}", bool(out))

    # 8.12: Landlock writes and seccomp-filtered syscalls (x86_64 numbers)
    for p in ["/etc/koinon-probe", "/usr/koinon-probe", "/var/koinon-probe", "/sandbox/../etc/koinon-probe"]:
        try_write("8.12", p)
    syscall("8.12", "unshare(CLONE_NEWUSER)", 272, 0x10000000)
    syscall("8.12", "unshare(CLONE_NEWNS)", 272, 0x00020000)
    syscall("8.12", "bpf(BPF_PROG_LOAD)", 321, 5, None, 0)
    syscall("8.12", "keyctl(GET_KEYRING_ID)", 250, 0, -3, 0)
    syscall("8.12", "add_key", 248, b"user", b"k", b"v", 1, -2)
    syscall("8.12", "perf_event_open", 298, None, 0, -1, -1, 0)

    opened = [r for r in results if r[2] == "OPEN"]
    print(f"\nSUMMARY: {len(results)} attempts, {len(opened)} OPEN")


main()
