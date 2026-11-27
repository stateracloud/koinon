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
import re
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


def client_hello(sni):
    """A TLS 1.2 ClientHello, with the server name when `sni` is a name (not an IP address). Any TLS server
    answers it with a ServerHello or an alert (bytes); the egress gateway answers with nothing."""
    import struct
    import os as _os
    ext = b""
    if sni and not re.fullmatch(r"[0-9.]+|[0-9a-fA-F]*:[0-9a-fA-F:.]*", sni):
        name = sni.encode()
        entry = b"\x00" + struct.pack(">H", len(name)) + name
        lst = struct.pack(">H", len(entry)) + entry
        ext += struct.pack(">HH", 0, len(lst)) + lst
    ext += struct.pack(">HHHH", 10, 4, 2, 23)  # supported_groups: secp256r1 (a list of length 2)
    ext += b"\x00\x0b\x00\x02\x01\x00"  # ec_point_formats: uncompressed
    ext += b"\x00\x0d\x00\x04\x00\x02\x04\x01"  # signature_algorithms: rsa_pkcs1_sha256
    suites = b"\xc0\x2f\xc0\x2b\x00\x9c\x00\x2f"
    body = b"\x03\x03" + _os.urandom(32) + b"\x00" + struct.pack(">H", len(suites)) + suites + b"\x01\x00"
    body += struct.pack(">H", len(ext)) + ext
    hs = b"\x01" + struct.pack(">I", len(body))[1:] + body
    return b"\x16\x03\x01" + struct.pack(">H", len(hs)) + hs


def payload(c):
    """Connects to host:port, sends bytes and reads the answer. A connect() alone proves nothing on
    Substrate: the actor's tunnel accepts every TCP connection and decides after it reads the Host or the
    SNI (ADR 0120 section 2.1, amendment of 2026-11-27, issue #1515). So the result is what came back:
      an errno name or "gaierror"   the connect or the send failed
      "closed"                      the peer closed with no byte (the gateway's refusal of a CONNECT)
      "timeout"                     no byte in 8 seconds
      "bytes N"                     N bytes arrived (kind "tls"): the destination, or something that
                                    pretends to be it, answered
      "status NNN server=... envoy=0|1 upstream=0|1"   (kind "http", as http_status)
    "kind" is "tls" (a ClientHello with `sni`, default: host) or "http" (GET / with `Host: host:port`).
    A byte answers "tls" only when it is the start of a TLS record."""
    try:
        infos = socket.getaddrinfo(c["host"], c["port"], type=socket.SOCK_STREAM)
    except OSError as e:
        return err_name(e)
    family, kind, proto, _, addr = infos[0]
    s = socket.socket(family, kind, proto)
    s.settimeout(8)
    try:
        s.connect(addr)
    except OSError as e:
        s.close()
        return err_name(e)
    data = b""
    try:
        if c.get("kind", "tls") == "http":
            host = "[%s]" % c["host"] if ":" in c["host"] else c["host"]
            s.sendall(("GET / HTTP/1.1\r\nHost: %s:%d\r\nConnection: close\r\n\r\n" % (host, c["port"])).encode())
        else:
            s.sendall(client_hello(c.get("sni", c["host"])))
        while len(data) < 8192 and (c.get("kind", "tls") != "http" or b"\r\n\r\n" not in data):
            chunk = s.recv(2048)
            if not chunk:
                break
            data += chunk
            if c.get("kind", "tls") != "http":
                break
        if not data:
            return "closed"
        if c.get("kind", "tls") != "http":
            return "bytes %d" % len(data)
        head = data.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
        parts = head[0].split(" ")
        if len(parts) < 2 or not parts[0].startswith("HTTP/"):
            return "bytes %d" % len(data)
        headers = {h.split(":", 1)[0].strip().lower(): h.split(":", 1)[1].strip() for h in head[1:] if ":" in h}
        envoy = int("envoy" in headers.get("server", "").lower() or any(k.startswith("x-envoy") for k in headers))
        upstream = int("x-envoy-upstream-service-time" in headers)
        return "status %s server=%s envoy=%d upstream=%d" % (parts[1], headers.get("server", "-"), envoy, upstream)
    except OSError as e:
        if data:
            return "bytes %d" % len(data)  # a byte came before the error: the destination was reached
        return err_name(e)
    finally:
        s.close()


def http_status(c):
    """GET / with `Host: host:port` through the egress gateway. The TCP connection goes to the address
    of "via" (a name the `*` rule lets through, resolved here), so it leaves the guest and the gateway
    resolves "host" itself; a name that the guest resolves to its own loopback is never connected to.
    Returns "status NNN server=<Server header or ->  envoy=<0|1> upstream=<0|1>", or the error name. A via address
    that is loopback or unspecified is reported as "badvia". The request carries the headers that the GCE
    (Metadata-Flavor) and Azure (Metadata) metadata servers require, so a server that is reached answers 200, so a check cannot pass without a gateway."""
    try:
        infos = socket.getaddrinfo(c["via"], c["port"], type=socket.SOCK_STREAM)
    except OSError as e:
        return err_name(e)
    family, kind, proto, _, addr = infos[0]
    if addr[0].startswith("127.") or addr[0] in ("0.0.0.0", "::1", "::"):
        return "badvia"
    s = socket.socket(family, kind, proto)
    s.settimeout(8)
    try:
        s.connect(addr)
        s.sendall(("GET / HTTP/1.1\r\nHost: %s:%d\r\nMetadata-Flavor: Google\r\nMetadata: true\r\nConnection: close\r\n\r\n" % (c["host"], c["port"])).encode())
        data = b""
        while b"\r\n\r\n" not in data and len(data) < 8192:
            chunk = s.recv(2048)
            if not chunk:
                break
            data += chunk
        head = data.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
        parts = head[0].split(" ")
        if len(parts) < 2 or not parts[0].startswith("HTTP/"):
            return "noresponse"
        headers = {h.split(":", 1)[0].strip().lower(): h.split(":", 1)[1].strip() for h in head[1:] if ":" in h}
        envoy = int("envoy" in headers.get("server", "").lower() or any(k.startswith("x-envoy") for k in headers))
        # x-envoy-upstream-service-time is set only on an answer that Envoy proxied from an upstream: the
        # gateway's own refusal never has it, so it marks a destination that was reached.
        upstream = int("x-envoy-upstream-service-time" in headers)
        return "status %s server=%s envoy=%d upstream=%d" % (parts[1], headers.get("server", "-"), envoy, upstream)
    except OSError as e:
        return err_name(e)
    finally:
        s.close()


def unix(c):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(5)
    try:
        s.connect(c["path"])
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
    environment and command line of every other process. A check may also give "pattern", a
    regular expression: a file whose content matches it is found too (a JWT shape, for example).
    The result names paths only, never the matched bytes."""
    needles = [bytes.fromhex(n) for n in c.get("needles", ())]
    pattern = re.compile(c["pattern"].encode()) if c.get("pattern") else None
    found, files = [], 0

    def check(path):
        nonlocal files
        try:
            with open(path, "rb") as f:
                data = f.read(64 << 20)
        except OSError:
            return
        files += 1
        if any(n in data for n in needles) or (pattern and pattern.search(data)):
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


def ids(_):
    """The real, effective and saved uid and gid, and the supplementary groups, from the system calls.
    The spec does not read them from /proc/self/status: the line format of that file can differ in
    gVisor."""
    return {"uid": list(os.getresuid()), "gid": list(os.getresgid()), "groups": os.getgroups()}


def open_write(c):
    """Opens an existing file for writing, with no create and no truncate, and closes it: "ok", or the
    errno name. The file is not changed, so the probe can test a platform file without harm."""
    try:
        os.close(os.open(c["path"], os.O_WRONLY))
        return "ok"
    except OSError as e:
        return err_name(e)


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
    if n == "landlock_abi":
        # landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION): the ABI number, or the errno
        # name (ENOSYS in gVisor, which has no Landlock; EOPNOTSUPP when it is disabled).
        rc = libc.syscall(444, None, 0, 1)
        return rc if rc >= 0 else sysresult(rc)
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
    if n == "vsock":
        return rawsock40(40)
    if n == "raw_inet":
        return rawsock(socket.AF_INET)
    raise ValueError(n)


def rawsock(family):
    try:
        socket.socket(family, socket.SOCK_RAW, 0).close()
        return "ok"
    except OSError as e:
        return err_name(e)


def rawsock40(family):
    # AF_VSOCK (40): the Substrate guest server's channel (docs/dev/guest-launcher.md).
    try:
        socket.socket(family, socket.SOCK_STREAM, 0).close()
        return "ok"
    except OSError as e:
        return err_name(e)


def _varint(n):
    out = b""
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out += bytes([b | 0x80])
        else:
            return out + bytes([b])


def _hpack_literal(name, value):
    # Literal header field without indexing, new name, no Huffman coding (RFC 7541 6.2.2).
    n, v = name.encode(), value.encode()
    assert len(n) < 127 and len(v) < 127
    return b"\x00" + bytes([len(n)]) + n + bytes([len(v)]) + v


def _frame(kind, flags, stream, payload):
    return len(payload).to_bytes(3, "big") + bytes([kind, flags]) + stream.to_bytes(4, "big") + payload


def grpc_h2c(c):
    """One gRPC call over cleartext HTTP/2 with no token: the guest front must refuse it.

    The result lists the frame types the server sent on stream 1 and on stream 0 (GOAWAY), so the
    spec can tell a refusal (a single HEADERS frame with END_STREAM: trailers-only) from an answer
    (DATA) and from a request the server could not parse (RST_STREAM or GOAWAY)."""
    msg = b"".join(bytes([0x0A]) + _varint(len(a.encode())) + a.encode() for a in c["command"])
    body = b"\x00" + len(msg).to_bytes(4, "big") + msg
    headers = b"".join(_hpack_literal(k, v) for k, v in [
        (":method", "POST"), (":scheme", "http"), (":path", c["path"]), (":authority", c["host"]),
        ("content-type", "application/grpc"), ("te", "trailers")])
    try:
        s = socket.create_connection((c["host"], c["port"]), timeout=5)
    except OSError as e:
        return err_name(e)
    frames = []
    try:
        s.sendall(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + _frame(4, 0, 0, b"")
                  + _frame(1, 0x4, 1, headers) + _frame(0, 0x1, 1, body))
        buf = b""
        while True:
            while len(buf) >= 9 and len(buf) >= 9 + int.from_bytes(buf[:3], "big"):
                n, kind, flags = int.from_bytes(buf[:3], "big"), buf[3], buf[4]
                stream = int.from_bytes(buf[5:9], "big") & 0x7FFFFFFF
                buf = buf[9 + n:]
                if kind == 4 and not flags & 0x1:
                    s.sendall(_frame(4, 0x1, 0, b""))  # ack the server's SETTINGS
                names = {0: "DATA", 1: "HEADERS", 3: "RST_STREAM", 7: "GOAWAY"}
                if stream == 1 or kind == 7:
                    frames.append(names.get(kind, str(kind)) + ("+END_STREAM" if kind in (0, 1) and flags & 0x1 else ""))
                    if kind in (0, 1) and flags & 0x1 or kind in (3, 7):
                        return frames
            chunk = s.recv(65536)
            if not chunk:
                frames.append("EOF")
                return frames
            buf += chunk
    except OSError as e:
        frames.append(err_name(e))
        return frames
    finally:
        s.close()


def grpc_http1(c):
    """The same call over HTTP/1.1. The guest front answers a refused gRPC call with the header
    grpc-status: 16 (Unauthenticated) in plain text; a gRPC server without the front refuses HTTP/1
    with no grpc-status (gRPC needs HTTP/2). The result is "<http status> <grpc-status or ->", or
    the errno name."""
    msg = b"".join(bytes([0x0A]) + _varint(len(a.encode())) + a.encode() for a in c["command"])
    body = b"\x00" + len(msg).to_bytes(4, "big") + msg
    req = ("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/grpc\r\nTE: trailers\r\n"
           "Content-Length: %d\r\nConnection: close\r\n\r\n" % (c["path"], c["host"], len(body))).encode() + body
    try:
        s = socket.create_connection((c["host"], c["port"]), timeout=5)
    except OSError as e:
        return err_name(e)
    try:
        s.sendall(req)
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
    except OSError as e:
        return err_name(e)
    finally:
        s.close()
    lines = buf.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
    code = lines[0].split(" ")[1] if len(lines[0].split(" ")) > 1 else "?"
    grpc = "-"
    for line in lines[1:]:
        name, _, value = line.partition(":")
        if name.strip().lower() == "grpc-status":
            grpc = value.strip()
    return code + " " + grpc


def sleep(c):
    """Waits c["seconds"]: lets a process that a call may have started do its work."""
    import time
    time.sleep(c["seconds"])
    return "ok"


def listeners(_):
    """The local TCP ports in LISTEN state (IPv4 and IPv6)."""
    ports = set()
    for path in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            with open(path) as f:
                for line in f.readlines()[1:]:
                    fields = line.split()
                    if fields[3] == "0A":
                        ports.add(int(fields[1].rsplit(":", 1)[1], 16))
        except OSError:
            pass
    return sorted(ports)


def egress_http(c):
    """One HTTP/1.1 GET of c["path"] to c["host"]:c["port"], with the headers of c["headers"] in their
    order ([name, value] pairs; a name can repeat). With c["tls"] the request goes through TLS, and the
    probe does not check the certificate: the spec asks what reaches the gateway, not whom it trusts.
    Returns {"status": NNN, "refusal": <Koinon-Egress-Refusal header or "">}, or the error name."""
    try:
        s = socket.create_connection((c["host"], c["port"]), timeout=10)
    except OSError as e:
        return err_name(e)
    try:
        if c.get("tls"):
            import ssl
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            ctx.minimum_version = ssl.TLSVersion.TLSv1_2
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            s = ctx.wrap_socket(s, server_hostname=c["host"])
        lines = ["GET %s HTTP/1.1" % c["path"], "Host: %s:%d" % (c["host"], c["port"]), "Connection: close"]
        lines += ["%s: %s" % (k, v) for k, v in c.get("headers", [])]
        s.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        data = b""
        while b"\r\n\r\n" not in data and len(data) < 16384:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
        head = data.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
        parts = head[0].split(" ")
        if len(parts) < 2 or not parts[0].startswith("HTTP/"):
            return "noresponse"
        headers = {h.split(":", 1)[0].strip().lower(): h.split(":", 1)[1].strip() for h in head[1:] if ":" in h}
        return {"status": int(parts[1]), "refusal": headers.get("koinon-egress-refusal", "")}
    except OSError as e:
        return err_name(e)
    finally:
        s.close()


def kernel(_):
    with open("/proc/cpuinfo") as f:
        hypervisor = any(l.startswith("flags") and " hypervisor" in l for l in f)
    return {"release": os.uname().release, "hypervisor": hypervisor}


OPS = {"tcp": tcp, "payload": payload, "http_status": http_status, "unix": unix, "udp_dns": udp_dns, "resolve": resolve, "read": read, "env": env, "scan": scan,
       "find": find, "mounts": mounts, "status": status, "ids": ids, "write": write, "open_write": open_write, "mode": mode,
       "syscall": syscall, "kernel": kernel, "grpc_h2c": grpc_h2c, "grpc_http1": grpc_http1, "sleep": sleep, "listeners": listeners,
       "egress_http": egress_http}
json.dump({c["id"]: OPS[c["op"]](c) for c in CHECKS}, sys.stdout)  # noqa: F821
