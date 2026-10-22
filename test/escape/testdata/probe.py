# SPDX-License-Identifier: Apache-2.0
"""Escape probe, run inside the sandbox as `python3 -` with this script on stdin.

The suite prepends CHECKS, a list of checks; the probe prints one JSON object mapping each
check's id to its result. Results name what happened (an errno name such as EACCES, "open",
"timeout") and never decide pass or fail: the Go specs do. Secret values arrive hex-encoded in
the script, so they are in no file, command line or environment the scan could find.
"""

import errno
import json
import os
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


OPS = {"tcp": tcp, "udp_dns": udp_dns, "resolve": resolve, "read": read, "env": env, "scan": scan}
json.dump({c["id"]: OPS[c["op"]](c) for c in CHECKS}, sys.stdout)  # noqa: F821
