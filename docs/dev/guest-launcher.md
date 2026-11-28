<!-- SPDX-License-Identifier: Apache-2.0 -->

# Guest launcher

- Status: Current
- Area: execution
- Tags: substrate, hardening, harness

`koinon-guest-launcher` (`cmd/koinon-guest-launcher`, `internal/guestlaunch`) hardens a Substrate
guest and then starts the agent (ADR 0119, 0120 section 4, issue #1087). It is a static Go binary in
the substrate overlay image (`make harness-substrate-<harness>`, `make harnesses-substrate` for all). The overlay also carries the guest front `koinon-guest-front` (`cmd/koinon-guest-front`, `internal/guestfront`, #1309), which is the image ENTRYPOINT and so the actor's process. The front embeds the upstream Substrate guest server (the package `github.com/agent-substrate/env/guest` at the commit in `go.mod`, unchanged) and serves its API behind a credential check. The release publishes it as `sandbox-substrate:<version>` (`hack/release-harness-substrate.sh`). The OpenShell driver does not use it.

## How it is invoked: an exec prefix

Substrate has no exec in its control API. The substrate driver runs the harness over the in-guest
ProcessService, so the process is a child of the guest server (in the guest front) and never of the harness ENTRYPOINT.
Every substrate-driver Exec of a harness, the ACP launch in particular, is therefore:

```
koinon-guest-launcher --policy /etc/koinon/guest-launcher/<policy of the tier> -- <EntryScript> ...
```

`harness.Descriptor.LauncherPrefix(tier)` returns the part before `<EntryScript>`; the driver
(#1088) prepends it. An unknown tier gets no prefix (the driver must not start the guest). OpenShell
paths ignore it. The overlay image's ENTRYPOINT is the guest front (`koinon-guest-front -workspace /sandbox`), not the
launcher: the guest front is the actor's process and the launcher only ever runs as this prefix.

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
   warning. Network rules are not used: they would change how agents reach their tools. The one
   scope is `landlock.scoped: ["signal"]` (ABI 6, Linux 6.12), in `policy-vm.json` only: the payload
   cannot send a signal to a process outside its Landlock domain (issue #1496). In `require` mode a
   kernel below ABI 6 stops the start. The abstract Unix socket scope is not used.
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

### The guest API channel

The driver reaches the guest API over the pod network, not over vsock: gRPC (`ProcessService` only: the guest
front has no file service, see Upload below) in TLS on port 443 of the atenet router (the client certificate is the pod certificate of agentd) and then plain HTTP/2 (h2c) from the router to TCP port 80 of the actor. The router
selects the actor by the `ate-target-actor` header and removes that header before it forwards
the call. The payload runs in the same network namespace in both tiers, so it can reach port 80 on
`127.0.0.1` or on the pod address. The upstream guest server has no authentication, so the guest
front owns the only listener (ADR 0120 section 4.7, issue #1309):

- The front embeds the upstream server in its own process. The upstream server opens no socket.
- Every call needs the header `x-koinon-guest-auth`: an Ed25519 JWS (`internal/guestauth`) that the
  driver signs for each call. It lives 60 seconds and its audience is `koinon-guest:<atespace>/<actor>`.
  A call without a valid token gets gRPC status `Unauthenticated` (over HTTP/1: the header `grpc-status: 16` for a gRPC request, HTTP 401 for any other) and reaches no
  handler. `/readyz` is open.
- The actor template carries the public keys in `KOINON_GUEST_VERIFY_KEYS`. The front reads them and
  removes the variable before it serves. The private key is the Secret `<release>-substrate-guest-key`
  (made by the ensure-secrets Job, or `execution.substrate.guestKey.existingSecret`), mounted only in
  agentd and the reconciler (`--substrate-guest-key-file`). Without it the driver makes no template.
- The front pins the audience of the first call it accepts and refuses every other audience. A sandbox
  restored from a memory snapshot has its source's pin in memory: the driver adds the claim
  `koinon_prev` with the source actor, and the front moves the pin only when that claim names the old
  pin. The driver calls `koinon.guestfront.v1.Front/Bind` before the first guest call to each actor
  and before each memory snapshot, so the pin is the snapshot's actor and one step of lineage is
  enough. For a memory snapshot of a suspended actor that the driver did not bind, it resumes the
  actor, binds it and suspends it again.
- A guest that answers `Bind` with `Unimplemented` has no front (an image from before #1309) and would
  accept unsigned calls: the driver refuses the sandbox before its first guest call.
- The front checks `iat` and `exp` against the guest clock, with 30 seconds of skew. A guest clock that
  does not step forward after a memory restore refuses every call (see Open items).
- The front writes at most one log line per 10 seconds about refused calls, with the number it left out.
- The escape suite (`test/escape`, "Substrate actor") calls `StartProcess` on loopback from the
  sandbox and expects a refusal, no process, and no other listener.

On the `vm` tier, vsock belongs to the Kata agent, not to the guest server; the seccomp rule below
keeps the payload away from it.

The guest server's own process, descriptors and sockets belong to it, not to the payload. The guest
server runs outside the launcher. On the gVisor tiers the payload runs as uid 10001 (see "The sandbox
user"). On the `vm` tier it can still run as uid 0 (issue #1462). So uid alone does not keep it out. Each way to a socket the payload did not create is closed:

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
its `fd` directory (a mode check only, no descriptor). The non-root user closes this on the gVisor
tiers. On the `vm` tier it stays open until issue #1462.

Exit 64 is a usage or policy error; exit 70 is a control that the policy requires and the guest
could not apply (fail closed).

## Policy

JSON, unknown fields refused. Each control has a `mode`: `require` (fail closed), `best_effort`
(apply when the kernel has it and log one clear line when not) or `off`. `seccomp.mode` must be
`require`: `off` and `best_effort` fail `Validate` (exit 64), because a skipped filter also skips the vsock and process-access denies (ADR 0120 fail closed). A file that names
`landlock` replaces the path lists. A policy whose `user.mode` is not `require` can leave the
payload root, so `Validate` refuses it when the allowlist names any capability except `CAP_NET_BIND_SERVICE`
and `CAP_AUDIT_WRITE` (`RootAllowedCapabilities`, issue #1496). After `capset` the launcher reads the sets back and exits 70
when a capability outside the allowlist is still permitted or effective.

The overlay image carries two policies that differ in the Landlock and user modes and the signal
scope only (ADR 0120 section 4.2 and 4.3, amendments of 2026-11-26 and 2026-11-27; the driver picks by
tier through `LauncherPrefix(tier)`):

| Tier | File | Landlock | User |
| --- | --- | --- | --- |
| `vm` | `cmd/koinon-guest-launcher/policy-vm.json` | `require` with the signal scope: no ABI 6 Landlock, no start (exit 70) | `best_effort`: root is accepted in v0.1 (issue #1496); the non-root drop is issue #1462 |
| `hardened`, `dev` (gVisor) | `cmd/koinon-guest-launcher/policy-gvisor.json` | `best_effort`: the kernel may not offer it | `require`: no drop to uid 10001, no start (exit 70) |

## The sandbox user

The payload runs as the sandbox user, uid and gid 10001 (the `sandbox` user of the harness images,
[images.md](images.md)). The launcher drops to it before it applies the filters: `setgroups([])`,
`setresgid`, then `setresuid`. The bounding set is shrunk before the drop (while root), and the
allowlist (empty in the image policies) is restored as effective, inheritable and ambient for the
non-root user. The default `user.mode` is `require` (issue #1387): when the drop fails, the launcher
exits 70 and the payload does not start. `best_effort` logs "user drop failed, continuing as the
current user" and applies every other control. A policy with `require` and uid 0 fails `Validate`.

**gVisor tiers (`hardened`, `dev`).** The actor template drops every capability of the agent container
and adds only `SETUID`, `SETGID`, `SETPCAP` and `KILL` (`substrate.GVisorCapAdd`). The guest front
uses `KILL` to signal the processes that it starts. The launcher uses the other three for the drop,
and the payload keeps none of them. The lab run of 2026-11-26 (issue #1387) shows, as uid 10001:
`CapEff` 0, `NoNewPrivs` 1, and `setuid(0)` returns `EPERM`, also in child processes. The guest front
still listens on port 80 without `NET_BIND_SERVICE`: the gVisor netstack has no privileged-port
check (gVisor commit `84708bc9b0a3`, `pkg/sentry/socket/netstack`, `pkg/tcpip/stack`). This is true only
when runsc uses its own netstack (`--network=sandbox`, the runsc default). With `--network=host` the host
kernel checks the port, and the front then needs `NET_BIND_SERVICE`.

The guest front stays root, but without `CAP_DAC_OVERRIDE` and `CAP_DAC_READ_SEARCH` it obeys the file
modes. Two results follow:

- **Upload** (#1478). The driver runs `mkdir -p DIR`, then `tar -x -f - -C DIR` as the payload user (behind the
  launcher), and sends the archive on the stdin of `tar`. The root front never writes a file, so no root process
  writes into a path that the payload can write (a symlink swap). The front starts the upstream guest server with
  the file service off (`EnableFileSystem: false`), so the guest has no `FileSystemService`. The archive goes in
  one `WriteProcessInput` stream of 256 KiB messages. The last message carries `close`. The driver then compares
  the bytes that the guest took with the bytes it sent. A guest process does not end when the stream that feeds
  it ends. So on a read error, a cancelled context or a broken stream (a suspend), the driver sends SIGKILL to
  `tar` by process id and never sends `close`. When the break came from a suspend, the kill waits until the
  actor runs again. `StartProcess` also has a 30 minute timeout as a backstop. The router resets a stream after
  300 s, so an upload that needs more time fails. `-log-dir` of the guest front is `/run/koinon/ate-jobs` by
  default. The front makes it and checks it before the payload starts, and fails when it is a symlink, has a symlink in its
  path or is writable by group or others. Do not set it to a directory that the payload can write: the front (root)
  writes the process logs there, and upstream follows symlinks (ADR 0120, amendment of 2026-11-27 for issue #1496, consequence "The root front writes only where the payload cannot write").
- **The home volume.** The front runs processes in `/sandbox`. The image makes
  `/sandbox` `10001:10001` mode `0750`, so root without `CAP_DAC_READ_SEARCH` cannot enter it. The owner
  and mode of the `DurableDir` volume at run time decide the result. The expected state is owner
  `10001:10001` and a mode that lets the front enter the directory (for example `0755`). The lab proof run
  of #1387 (2026-11-26, head `41a2400b5`) showed `--network=sandbox`, Exec in `/sandbox` as uid 10001
  with `CapEff` 0, `/sandbox` at `0:0` mode 777 and 14 of 18 escape specs (the 4 network failures are the
  same on main; the cause is not confirmed). The Upload and restore proof belongs to #1478.

**`vm` tier.** The template keeps the Substrate default set. The Substrate v1.0.1 microVM guest gave
payloads root with only `CAP_KILL`, `CAP_NET_BIND_SERVICE` and `CAP_AUDIT_WRITE`, so the drop failed
there (`setresgid: operation not permitted`; phase 0 result, comment on #1087). `policy-vm.json` keeps
`user.mode: best_effort` until the lab proves the drop on the `vm` tier (issue #1462). As root the
payload keeps the guest's bounding set when the guest gives no `CAP_SETPCAP`; the permitted and
effective sets are reduced to the allowlist.

**Root on the `vm` tier (issue #1496, ADR 0120 amendment of 2026-11-27).** In v0.1 the payload may run
as uid 0 in the microVM guest. The guest kernel under hardware virtualisation is the host boundary.
Inside the guest, the launcher keeps the payload away from the Koinon processes (the guest front and
the processes that it starts outside the launcher):

| Capability or path | How the payload loses it | Harness need |
| --- | --- | --- |
| `CAP_SYS_PTRACE` | Empty allowlist; `ptrace`, `process_vm_readv`, `process_vm_writev`, `pidfd_getfd` and `kcmp` denied by seccomp; Landlock refuses a trace of a process outside the domain | None. A debugger in the sandbox does not work on any tier. |
| `CAP_SYS_ADMIN` | Empty allowlist; `mount`, the new mount API, `setns`, `unshare` and namespace flags of `clone` denied by seccomp | None. |
| `CAP_SYS_MODULE` | Empty allowlist; `init_module`, `finit_module`, `delete_module` denied by seccomp | None. |
| `CAP_NET_RAW` | Empty allowlist | None. `ping` does not work; egress is HTTP(S) through the gateway. |
| `CAP_SYS_BOOT` | Empty allowlist; `reboot`, `kexec_load`, `kexec_file_load` denied by seccomp | None. |
| `CAP_KILL` | Empty allowlist | None: the payload signals its own processes without it. |
| Same-uid signals | Landlock signal scope: no signal (`kill`, `tgkill`, `pidfd_send_signal`, `SIGIO` through `F_SETOWN`) reaches a process outside the payload's domain. Uid 0 needs no capability to signal another uid 0 process, so the scope is the control here. | The payload and its children share one domain and signal each other. A process of one exec cannot signal a process of another exec. |
| `CAP_SYS_RAWIO` | Empty allowlist; `ioperm`, `iopl` denied by seccomp; Landlock names the device nodes one by one | None. |

`no_new_privs` caps the permitted set of every later `execve` at the one before it, so a root payload
whose bounding set did not shrink does not get its capabilities back from an exec of a root or
setuid file. OpenCode, git, and package installs into the home and `/tmp` (npm, pip, go) need no
capability. A system package install (`apt`, `dpkg`) writes to `/usr` and `/var/lib`, which Landlock
keeps read-only on every tier, so it does not work in either case.

The home files differ in owner between tiers: uid 0 on the `vm` tier, uid 10001 on the gVisor
tiers. A home that moves between tiers (a restore from a snapshot of the other tier) keeps its owner:
files that root made are owned by uid 0, and uid 10001 can write them only where the mode allows it.

The container spec "the image policies in the shape the Substrate guest gives a payload" proves the
`vm` row in Docker (root with only `KILL`, `NET_BIND_SERVICE` and `AUDIT_WRITE`): `kill_other` and
`pidfd_signal_other` (signal 0 to an outside root process), `ptrace_other`, `mount`, `finit_module`,
`reboot` and `socket_raw` are blocked, `kill_own_child` works, `CapPrm`, `CapEff` and `CapAmb` are 0. The
proof in a microVM guest needs KVM and is a lab run.

## gVisor tiers

On the hardened and dev tiers (gVisor) the launcher gets `policy-gvisor.json`, with Landlock
`best_effort`: the kernel may not offer it (maintainer decision on #1087). The
gVisor tiers keep a **writable root filesystem** by maintainer decision (2026-11-19, issue
[#1150](https://github.com/stateracloud/koinon/issues/1150)). Substrate v1.0.1 sets the root of an actor
container writable, and a read-only root breaks `MEMORY` snapshots (`runsc checkpoint: no checkpointable
filesystems`). Koinon carries no Substrate patch. So without Landlock, a payload on these tiers can write to
`/etc`, `/usr` and `/var`. The launcher's other controls (`no_new_privs`, capability drop, seccomp) stay. When
Landlock is unavailable the launcher logs one warning that says so (ADR 0120 section 4, item 3: the `vm` tier
fails closed through `policy-vm.json`, another tier records the gap). Each Substrate update reviews #1150.

## Open items

Tracked on #1087 until the maintainer files separate issues:

1. The substrate overlay of the harness image carries the guest front and the launcher
   (`cmd/koinon-guest-launcher/Dockerfile`). The guest front embeds the upstream guest server of the
   commit in `go.mod`; a test keeps `ATE_ENV_VERSION` (`hack/build-env-guest.sh`) on the same commit. The release builds the overlay on the
   released harness image (`harness-substrate` job, `hack/release-harness-substrate.sh`, repository `sandbox-substrate`) and with
   `execution.driver=substrate` the chart's default harness repository is `sandbox-substrate`. A release publishes no separate `ate-env-guest`
   image. The rootfs-image mode of upstream (the guest mounted into an unmodified image) would need that image again; `make env-guest` and `hack/build-env-guest.sh` still build the binary for it. The driver does not use that mode.
   The overlay runs as root (the guest front listens on port 80 and starts the payload), the launcher hardens the payload.
   The driver applies the exec prefix to every Exec (`internal/execution/substrate`, `Config.RuntimeClassTiers`).
2. Probe results in the Substrate microVM and in gVisor classes are a lab run, not yet recorded.
   `execution.substrate.allowUnhardened` stays required until they are (see the PR body of #1087).
3. Read-only root on the gVisor tiers: decided against for now. See "gVisor tiers" above and issue #1150
   (review at each Substrate update).
4. Non-root payload user: done on the gVisor tiers (issue #1387). On the `vm` tier, `user.mode` stays
   `best_effort` until issue #1462, and the payload can run as root in its microVM with every other
   control applied.
5. Rotation of the guest API signing key is not supported yet (#1326): the front accepts a list of keys, but
   the chart and the driver have one key. A new key (the Secret deleted, or a new install with old
   snapshots) makes every running sandbox and every memory snapshot that has the old public key unusable.
6. The token check uses the guest clock. On the gVisor tiers the guest uses host time. On the `vm` tier,
   a memory restore must step the guest clock forward; the lab probe record (ADR 0119 P-C) must confirm it.
7. On the gVisor tiers without Landlock (best effort), the root filesystem is writable (#1150). The
   payload runs as uid 10001 (issue #1387), so it cannot replace `koinon-guest-launcher` or its policy
   file where the image makes them owned by root and not writable by others. The escape spec "runs the
   agent as the non-root sandbox user, who cannot replace the launcher or its policy"
   (`test/escape/substrate_test.go`) checks this. The proof run on the lab (issue #1387) must pass it
   with the real front image before `allowUnhardened` is removed.

## Tests

`go test ./internal/guestlaunch` (Ginkgo): policy parsing, the filter run through the BPF VM, and
(`Label("docker")`, needs Docker) the launcher in a scratch container with a Go port of
`hack/spike/probe`: each attempt is open without the launcher and blocked with it; the user drop
succeeds, degrades and fails closed. The container specs also run each image policy in the shape
the Substrate guest gives a payload on its tier: root with `--cap-drop=ALL` plus `KILL`,
`NET_BIND_SERVICE`, `AUDIT_WRITE` (`vm`), or plus `KILL`, `SETUID`, `SETGID`, `SETPCAP` (gVisor).
`CapPrm`, `CapEff` and `CapAmb` are 0 and `NoNewPrivs` is 1; on the gVisor tiers the payload is uid
10001 and `CapBnd` is 0. `policy-gvisor.json` in the `vm` shape (no `CAP_SETUID`) exits 70 and
starts no payload. With Landlock hidden by a Docker seccomp profile, `policy-vm.json` exits 70 and
`policy-gvisor.json` starts. The probe
tries `clone(CLONE_NEWUSER)`, `clone3`, `io_uring_setup`, `socket(AF_VSOCK)`, creating a file or
a block device node in `/dev`, opening an unlisted node and, when the host gives one, a block
device. Before the launcher, the probe plays the guest server (`probe inherit --`): it leaves a
socket at fd 7 without CLOEXEC and starts an outside process that holds a socket; the payload then
tries the inherited fd, `/proc/<pid>/fd/3`, `environ`, `ptrace`, `process_vm_readv` and
`pidfd_getfd` on that process, also with `close_range` and with Landlock hidden. The specs skip on a kernel with Landlock below ABI 5. Probe results in a microVM and in gVisor are a lab run, recorded
on #1087.
