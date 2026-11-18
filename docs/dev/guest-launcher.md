<!-- SPDX-License-Identifier: Apache-2.0 -->

# Guest launcher

- Status: Current
- Area: execution
- Tags: substrate, hardening, harness

`koinon-guest-launcher` (`cmd/koinon-guest-launcher`, `internal/guestlaunch`) hardens a Substrate
guest and then starts the agent (ADR 0119, 0120 section 4, issue #1087). It is a static Go binary in
the substrate overlay image (`make harness-substrate-opencode`). The OpenShell driver does not use it.

## How it is invoked: an exec prefix

Substrate has no exec in its control API. The substrate driver runs the harness over the in-guest
ProcessService, so the process is a child of the guest server and never of the image ENTRYPOINT.
Every substrate-driver Exec of a harness, the ACP launch in particular, is therefore:

```
koinon-guest-launcher --policy /etc/koinon/guest-launcher/policy.json -- <EntryScript> ...
```

`harness.Descriptor.LauncherPrefix` carries the part before `<EntryScript>`; the driver (#1088)
prepends it. OpenShell paths ignore it. The overlay image keeps the same line as its ENTRYPOINT, as a
default only: it alone does not harden a ProcessService exec.

```
koinon-guest-launcher [--policy FILE] [--check] -- COMMAND [ARG...]
```

Order of steps, all on one locked OS thread, then `execve`:

1. `no_new_privs`.
2. User drop to the policy's uid and gid (`setgroups`, `setresgid`, `setresuid`).
3. Capability allowlist: bounding set (needs `CAP_SETPCAP`), then permitted, effective, inheritable
   and ambient. An empty list drops everything.
4. Landlock filesystem rules (V7 and best effort down to the kernel's ABI; the kernel ABI is read
   with `landlock_create_ruleset(NULL, 0, VERSION)` and logged). Network and scoped rules are not
   used: they would change how agents reach their tools.
5. seccomp deny filter (`internal/guestlaunch/seccomp.go`): the deny list returns `EPERM`; `clone`
   with a namespace flag returns `EPERM`; `clone3` returns `ENOSYS` so glibc falls back to `clone`
   (as Docker's default profile does); a call from a foreign architecture (the i386 compat ABI
   through `int 0x80`, AArch32) returns `ENOSYS`; x32 is refused by go-seccomp-bpf.

Exit 64 is a usage or policy error; exit 70 is a control that the policy requires and the guest
could not apply (fail closed).

## Policy

JSON, unknown fields refused. Each control has a `mode`: `require` (fail closed), `best_effort`
(apply when the kernel has it and log one clear line when not) or `off`. The image policy is
`cmd/koinon-guest-launcher/policy.json`. A file that names `landlock` replaces the path lists.

## The non-root gap

The Substrate v1.0.0 guest gives payloads root with only `CAP_KILL`, `CAP_NET_BIND_SERVICE` and
`CAP_AUDIT_WRITE`. Without `CAP_SETUID` and `CAP_SETGID` the launcher cannot drop to uid 10001
(`setresgid: operation not permitted`; phase 0 result, comment on #1087). The default `user.mode`
is `best_effort`: the launcher logs "user drop failed, continuing as the current user" and applies
every other control. `user.mode: require` fails closed. Closing the gap needs an upstream Substrate
option for the payload uid/gid or capabilities, or a different design. As root the payload also keeps
the guest's bounding set when the guest gives no `CAP_SETPCAP`; the permitted and effective sets are
reduced to the allowlist. When the drop works, the bounding set is shrunk before it (while root),
and the allowlist is restored as effective, inheritable and ambient for the non-root user.

## gVisor tiers

On the hardened and dev tiers (gVisor) Landlock stays `best_effort`: the kernel may not offer it. The
compensating control is a read-only root filesystem. Driver requirement for #1088: mount the root
read-only, with only `/sandbox` and `/tmp` writable, so writes to `/etc`, `/usr` and `/var` stay
blocked. When Landlock is unavailable the launcher logs one warning that names the read-only root as
the control (ADR 0120 section 4, item 3: the `vm` tier fails closed, another tier records the gap).

## Open items

Tracked on #1087 until the maintainer files separate issues:

1. Flavor images carry the launcher only; the guest server joins them with #1088, which also
   applies the exec prefix.
2. Probe results in the Substrate microVM and in gVisor classes are a lab run, not yet recorded.
3. #1088 must mount the root filesystem read-only on gVisor tiers (see "gVisor tiers" above).
4. Non-root payload user: waits for an upstream Substrate payload uid/gid option (Koinon issue to be
   filed). Until then `user.mode` stays `best_effort` and the payload runs as root in its microVM with
   every other control applied; `require` still fails closed.

## Tests

`go test ./internal/guestlaunch` (Ginkgo): policy parsing, the filter run through the BPF VM, and
(`Label("docker")`, needs Docker) the launcher in a scratch container with a Go port of
`hack/spike/probe`: each attempt is open without the launcher and blocked with it; the user drop
succeeds, degrades and fails closed. Probe results in a microVM and in gVisor are a lab run, recorded
on #1087.
