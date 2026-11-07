# SPDX-License-Identifier: Apache-2.0
"""Spike step 2 probe: look for real credentials from inside a sandbox.

The probe never knows the real value. It collects every 40-hex-character string
(the Gitea token format), plus base64 blobs that decode to one, from the
environment, readable files and readable process memory, and prints only their
SHA-256 hashes with where they were found. The caller compares the hashes with
the real token's hash outside the sandbox.
"""

import base64
import hashlib
import json
import os
import re
import sys

HEX40 = re.compile(rb"(?<![0-9a-f])[0-9a-f]{40}(?![0-9a-f])")
B64 = re.compile(rb"[A-Za-z0-9+/]{52,}={0,2}")
SKIP_DIRS = ("/proc", "/sys", "/dev")
# Landlock denies listing "/", so walk the standard top-level directories one by one.
ROOTS = ["/bin", "/boot", "/etc", "/home", "/lib", "/lib64", "/media", "/mnt", "/opt", "/root",
         "/run", "/sandbox", "/sbin", "/srv", "/tmp", "/usr", "/var", "/.openshell"]
MAX_FILE = 64 << 20

found = {}  # sha256 -> set of locations
stats = {"files": 0, "file_errors": 0, "procs": 0, "proc_errors": [], "placeholders": {}}


def scan(data, where):
    for m in HEX40.finditer(data):
        found.setdefault(hashlib.sha256(m.group()).hexdigest(), set()).add(where)
    for m in B64.finditer(data):
        try:
            dec = base64.b64decode(m.group(), validate=False)
        except Exception:
            continue
        for h in HEX40.finditer(dec):
            found.setdefault(hashlib.sha256(h.group()).hexdigest(), set()).add(where + " (base64)")


def scan_env():
    for k, v in os.environ.items():
        if v.startswith("openshell:resolve:env:"):
            stats["placeholders"][k] = v.split(":")[3] if v.count(":") >= 3 else "?"
        scan(v.encode(), "env:" + k)


def scan_files():
    for root in ROOTS:
        try:
            os.listdir(root)
            stats.setdefault("roots_listed", []).append(root)
        except OSError as e:
            stats.setdefault("roots_denied", []).append(f"{root}:{e.strerror}")
            continue
        walk(root)


def walk(root):
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        if dirpath.startswith(SKIP_DIRS):
            dirnames[:] = []
            continue
        for name in filenames:
            path = os.path.join(dirpath, name)
            try:
                if not os.path.isfile(path) or os.path.islink(path) or os.path.getsize(path) > MAX_FILE:
                    continue
                with open(path, "rb") as f:
                    scan(f.read(), "file:" + path)
                stats["files"] += 1
            except OSError:
                stats["file_errors"] += 1


def use_placeholder():
    """Send the placeholder to Gitea, so the response passes through this process."""
    import urllib.request
    req = urllib.request.Request("https://git.example.invalid/api/v1/user",
                                 headers={"Authorization": "Bearer " + os.environ.get("SPIKE_TOKEN", "")})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            body = r.read()
            stats["request"] = {"status": r.status, "login": json.loads(body).get("login")}
    except Exception as e:
        stats["request"] = {"error": str(e)[:200]}


def scan_procs():
    for pid in sorted(int(p) for p in os.listdir("/proc") if p.isdigit()) + ["self"]:
        if pid == os.getpid():
            continue
        try:
            with open(f"/proc/{pid}/comm") as f:
                comm = f.read().strip()
        except OSError:
            continue
        for part in ("environ", "cmdline"):
            try:
                with open(f"/proc/{pid}/{part}", "rb") as f:
                    scan(f.read(), f"proc:{pid}:{comm}:{part}")
            except OSError as e:
                stats["proc_errors"].append(f"{pid}:{comm}:{part}:{e.strerror}")
        try:
            with open(f"/proc/{pid}/maps") as maps, open(f"/proc/{pid}/mem", "rb", 0) as mem:
                for line in maps:
                    addr, perms = line.split()[:2]
                    if "r" not in perms:
                        continue
                    start, end = (int(x, 16) for x in addr.split("-"))
                    if end - start > 1 << 30:
                        continue
                    try:
                        mem.seek(start)
                        scan(mem.read(end - start), f"proc:{pid}:{comm}:mem")
                    except (OSError, ValueError, OverflowError):
                        pass
            stats["procs"] += 1
        except OSError as e:
            stats["proc_errors"].append(f"{pid}:{comm}:mem:{e.strerror}")


scan_env()
use_placeholder()
if "--no-files" not in sys.argv:
    scan_files()
scan_procs()
print(json.dumps({
    "uid": os.getuid(),
    "stats": stats,
    "hashes": {h: sorted(w) for h, w in sorted(found.items())},
}, indent=1))
