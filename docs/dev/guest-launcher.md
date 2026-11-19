<!-- SPDX-License-Identifier: Apache-2.0 -->

# Guest launcher

- Status: Current
- Area: execution
- Tags: substrate, hardening, harness

`koinon-guest-launcher` (`cmd/koinon-guest-launcher`, `internal/guestlaunch`) hardens a Substrate
guest and then starts the agent (ADR 0119, 0120 section 4, issue #1087). It is a static Go binary in
the substrate overlay image (`make harness-substrate-<harness>`, `make harnesses-substrate` for all). The OpenShell driver does not use it.

## How it is invoked: an exec prefix

Substrate has no exec in its control API. The substrate driver runs the harness over the in-guest
ProcessService, so the process is a child of the guest server and never of the image ENTRYPOINT.
Every substrate-driver Exec of a harness, the ACP launch in particular, is therefore:

```
koinon-guest-launcher --policy /etc/koinon/guest-launcher/<policy of the tier> -- <EntryScript> ...
```

`harness.Descriptor.LauncherPrefix(tier)` returns the part before `<EntryScript>`; the driver
(#1088) prepends it. An unknown tier gets no prefix (the driver must not start the guest). OpenShell
paths ignore it. The overlay image keeps the same line as its ENTRYPOINT, as a
default only: it alone does not harden a ProcessService exec.

```
koinon-guest-launcher [--policy FILE] [--check] -- COMMAND [ARG...]
```

Order of steps, all on one locked OS thread, then `execve`:

0. Every descriptor above 2 is marked close-on-exec (`close_range(3, ~0U, CLOSE_RANGE_CLOEXEC)`;
   where the kernel lacks it (`ENOSYS`, `EINVAL`) or an outer seccomp profile answers `EPERM`, `FD_CLOEXEC` on each entry of `/proc/self/fd`; neither works: exit
   70). The `execve` then closes them all, so nothing the guest server left open reaches the payload.
1. `no_new_privs`.
2. User drop to the policy's uid and gid (`setgroups`, `setresgid`, `setresuid`).
3. Capability allowlist: bounding set (needs `CAP_SETPCAP`), then permitted, effective, inheritable
   and ambient. An empty list drops everything.
4. Landlock filesystem rules, pinned to ABI 5 (V5 adds ioctl on device files; the kernel ABI is read
   with `landlock_create_ruleset(NULL, 0, VERSION)` and logged). In `require` mode a kernel with
   ABI below 5, or none, stops the start; in `best_effort` mode the rules are skipped with one
   warning. Network and scoped rules are not used: they would change how agents reach their tools.
   Devices are named one by one: `/dev/null`, `zero`, `full`, `random`, `urandom`, `tty`, `ptmx`,
   `pts`, `shm` get read, write and ioctl. `/dev` itself can only be listed (`list_dirs`), so a block
   device or any other node cannot be opened, and none can be created.
5. seccomp deny filter (`internal/guestlaunch/seccomp.go`): the deny list returns `EPERM`; `clone`
   with a namespace flag returns `EPERM`; `clone3` returns `ENOSYS` so glibc falls back to `clone`
   (as Docker's default profile does); `io_uring_setup`, `io_uring_enter` and `io_uring_register`
   return `EPERM` (io_uring runs operations outside the checks of the calls it replaces);
   `socket(AF_VSOCK)` returns `EPERM`; a call from a foreign architecture (the i386 compat ABI
   through `int 0x80`, AArch32) returns `ENOSYS`; x32 (a number from 0x40000000 up on x86_64) returns `ENOSYS`, from go-seccomp-bpf.

Every name in `seccomp.deny` and `deny_extra` is checked against the syscall names of all
architectures the filter library knows: a name none has fails `Validate`; a name only another
architecture has is skipped on this one and logged. `ptrace`, `process_vm_readv`,
`process_vm_writev`, `pidfd_getfd` and `kcmp` stay denied when a policy replaces `seccomp.deny`.

### The vsock channel

The vsock channel belongs to the guest server, not to the payload. The guest server runs outside the
launcher and keeps its own vsock sockets; the payload runs as uid 0 until the non-root gap closes, so
uid alone does not keep it out. Each way to a socket the payload did not create is closed:

| Way | Control |
| --- | --- |
| `socket(AF_VSOCK)` | seccomp, `EPERM` |
| A descriptor inherited without CLOEXEC | step 0 above: closed by the `execve` |
| `open("/proc/<pid>/fd/<n>")` of a socket | the kernel: a socket cannot be opened through its fd link (`ENXIO`, even for unconfined root; `sock_no_open` in `net/socket.c`) |
| `pidfd_getfd`, `ptrace`, `process_vm_readv`/`writev`, `kcmp` | seccomp, `EPERM`; `Validate` refuses a policy whose `seccomp.mode` is not `require`, so no policy skips them |
| `/proc/<pid>/mem`, `environ` and fd links of the guest server | the kernel's ptrace access check: the payload has no capability, so the guest server's permitted set is not a subset of its own (`cap_ptrace_access_check`); on the `vm` tier Landlock also refuses ptrace access to a process outside its domain |

`pidfd_open` stays allowed: a pidfd only signals and waits (signals need no pidfd for the same uid),
and the Go runtime and libuv use it for their children. Landlock keeps read on all of `/proc`:
Landlock binds a rule to the inode a path names when the rule is added, so `/proc/self` resolves
once, to the launcher's own `/proc/<pid>`. The payload keeps that pid across the `execve`, but each
process it starts (a shell, git, a Node worker) has another pid, and such a rule leaves every child
without its own `/proc/self` (tested: the child gets `EACCES`). Landlock cannot express "the
caller's own pid"; the per-process entries that matter are guarded by the ptrace access check above.
A uid-0 payload can still read the guest server's `status` and `cmdline`, and list the numbers in
its `fd` directory (a mode check only, no descriptor); that closes with the non-root user.

Exit 64 is a usage or policy error; exit 70 is a control that the policy requires and the guest
could not apply (fail closed).

## Policy

JSON, unknown fields refused. Each control has a `mode`: `require` (fail closed), `best_effort`
(apply when the kernel has it and log one clear line when not) or `off`. `seccomp.mode` must be
`require`: `off` and `best_effort` fail `Validate` (exit 64), because a skipped filter also skips the vsock and process-access denies (ADR 0120 fail closed). A file that names
`landlock` replaces the path lists.

The overlay image carries two policies that differ in the Landlock mode only (ADR 0120 section 4.2
and 4.3; the driver picks by tier through `LauncherPrefix(tier)`):

| Tier | File | Landlock |
| --- | --- | --- |
| `vm` | `cmd/koinon-guest-launcher/policy-vm.json` | `require`: no ABI 5 Landlock, no start (exit 70) |
| `hardened`, `dev` (gVisor) | `cmd/koinon-guest-launcher/policy-gvisor.json` | `best_effort`: the read-only root is the control |

## The non-root gap

The Substrate v1.0.0 guest gives payloads root with only `CAP_KILL`, `CAP_NET_BIND_SERVICE` and
`CAP_AUDIT_WRITE`. Without `CAP_SETUID` and `CAP_SETGID` the launcher cannot drop to uid 10001
(`setresgid: operation not permitted`; phase 0 result, comment on #1087). The default `user.mode`
is `best_effort`: the launcher logs "user drop failed, continuing as the current user" and applies
every other control. `user.mode: require` fails closed. Tracked on #1087 (needs a payload uid/gid
option in Substrate). As root the payload also keeps
the guest's bounding set when the guest gives no `CAP_SETPCAP`; the permitted and effective sets are
reduced to the allowlist. When the drop works, the bounding set is shrunk before it (while root),
and the allowlist is restored as effective, inheritable and ambient for the non-root user.

## gVisor tiers

On the hardened and dev tiers (gVisor) the launcher gets `policy-gvisor.json`, with Landlock
`best_effort`: the kernel may not offer it (maintainer decision on #1087). The
compensating control is a read-only root filesystem. Driver requirement for #1088: mount the root
read-only, with only `/sandbox` and `/tmp` writable, so writes to `/etc`, `/usr` and `/var` stay
blocked. When Landlock is unavailable the launcher logs one warning that names the read-only root as
the control (ADR 0120 section 4, item 3: the `vm` tier fails closed through `policy-vm.json`,
another tier records the gap).

## Open items

Tracked on #1087 until the maintainer files separate issues:

1. Flavor images carry the launcher only. The guest server image `ate-env-guest` has no public build:
   Koinon builds it from the Substrate source and pins it by digest (ADR 0061 image rules). That needs the
   upstream source and a registry, and waits for the lab (see the PR body of #1087). The driver
   applies the exec prefix to every Exec (`internal/execution/substrate`, `Config.RuntimeClassTiers`).
2. Probe results in the Substrate microVM and in gVisor classes are a lab run, not yet recorded.
3. #1088 must mount the root filesystem read-only on gVisor tiers (see "gVisor tiers" above).
4. Non-root payload user: tracked on #1087 (needs a payload uid/gid option in Substrate). Until then `user.mode` stays `best_effort` and the payload runs as root in its microVM with
   every other control applied; `require` still fails closed.

## Tests

`go test ./internal/guestlaunch` (Ginkgo): policy parsing, the filter run through the BPF VM, and
(`Label("docker")`, needs Docker) the launcher in a scratch container with a Go port of
`hack/spike/probe`: each attempt is open without the launcher and blocked with it; the user drop
succeeds, degrades and fails closed. The container specs also run both image policies in the shape
the Substrate guest gives a payload (root, `--cap-drop=ALL` plus `KILL`, `NET_BIND_SERVICE`,
`AUDIT_WRITE`): `CapPrm`, `CapEff` and `CapAmb` are 0 and `NoNewPrivs` is 1; and with Landlock hidden
by a Docker seccomp profile, `policy-vm.json` exits 70 and `policy-gvisor.json` starts. The probe
tries `clone(CLONE_NEWUSER)`, `clone3`, `io_uring_setup`, `socket(AF_VSOCK)`, creating a file or
a block device node in `/dev`, opening an unlisted node and, when the host gives one, a block
device. Before the launcher, the probe plays the guest server (`probe inherit --`): it leaves a
socket at fd 7 without CLOEXEC and starts an outside process that holds a socket; the payload then
tries the inherited fd, `/proc/<pid>/fd/3`, `environ`, `ptrace`, `process_vm_readv` and
`pidfd_getfd` on that process, also with `close_range` and with Landlock hidden. The specs skip on a kernel with Landlock below ABI 5. Probe results in a microVM and in gVisor are a lab run, recorded
on #1087.
