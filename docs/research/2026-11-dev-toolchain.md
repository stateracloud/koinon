# Dev toolchain: one source for tool versions, and the bash 3.2 gap

- Status: Complete
- Date: 2026-11-02
- Area: engineering-process
- Tags: tooling, dependencies, ci

Research date: 2026-11-02, for #536. The issue started as a Nix flake trial; procoduck widened it to
"which option is best", so six options are scored on the same criteria. Probes ran on procoduck's Mac
(macOS 27.2, arm64) inside the Claude Code sandbox, with nothing installed system-wide: mise and pkgx
as single binaries in `$TMPDIR`, Nix through the multi-user install that was already there (2.21.1,
April 2024). The trial `flake.nix` and `flake.lock` were removed again (see Decision); they are in the git history of the trial branch only.

## Decision

2026-11-02, procoduck: no Nix. He has uninstalled it from his Mac and will not reinstall it. The winner,
"stay, with one versions file, a drift check and a bash 3.2 CI job" (option 6), is filed as #544.
The Nix flake was the runner-up and is the only option that removes the bash 3.2 gap at the source.
Revisit it if the #544 bash 3.2 CI job proves too slow or misses BSD/GNU differences. The trial
`flake.nix` and `flake.lock` are removed; this note is the record.

## Short answer

**Winner: stay, but make one versions file the source (option 6).** It is the only option that works
everywhere today (Mac sandbox, cloud session, CI) without a new install, and every other option needs
it anyway: CI and `hack/e2e-toolchain/Dockerfile` keep their pinned downloads whichever dev shell
contributors use. It *catches* the bash 3.2 / BSD gap in CI rather than removing it.

**Runner-up: the Nix flake (option 1).** It is the only option that *removes* the gap on every
machine (bash 5.3, GNU coreutils/sed/grep/awk) and pins every tool exactly; at the pinned revision
nixpkgs already has every version the repository pins. What it costs: every contributor installs
Nix; agents on the Mac need a sandbox rule for the daemon socket (refused today); a CI job pays a
Nix install plus a closure download; someone bumps `flake.lock` and checks its age. If we take option
6 now, adding the flake later costs little: the versions file becomes the input that the flake's
`exactPins` overlay and a drift check read.

## The problem today

Tool versions live in five places: `go.mod` (`go 1.27.3` and the `tool (...)` block), the
CONTRIBUTING.md table, `hack/e2e-toolchain/Dockerfile` (Go, kind, ko, helm, kubectl with sha256s),
workflow env (`KIND_VERSION` in `g2-kind.yml` and `e2e-kind.yml`, `KO_VERSION`, `GITLEAKS_VERSION` in
`secrets.yml`, `version: v4.3.1` for helm and `v2.15.0` for golangci-lint in `ci.yml` steps) and hack
scripts (`GOFUMPT_VERSION` in `hack/preflight.sh`, `KUBECONFORM_VERSION` in `hack/chart-check.sh`).
shellcheck and actionlint are not pinned anywhere. `#!/usr/bin/env bash` is `/bin/bash` 3.2.57 on
the Mac (checked) and bash 5 in CI.

## Versions: repository pins against each option (2026-11-02)

| Tool | Repo pin (where) | nixpkgs `31ecbac` | Homebrew stable | pkgx pantry | mise (aqua) |
| --- | --- | --- | --- | --- | --- |
| Go | 1.27.3 (go.mod, Dockerfile) | 1.27.3 (`go_1_27`; default `go` is 1.26) | 1.27.3 | 1.27.3 | exact |
| helm | v4.3.1 (Dockerfile, ci.yml, CONTRIBUTING) | 4.3.1 | 4.3.1 | 4.3.1 | exact |
| kind | v0.33.0 (Dockerfile, 3 workflows) | 0.33.0 | 0.33.0 | 0.33.0 | exact |
| kubectl | v1.37.0 (Dockerfile; "within one minor") | 1.37.1 | 1.37.2 | not checked | exact |
| ko | 0.19.1 (Dockerfile, e2e-kind.yml) | 0.19.1 | 0.19.1 | 0.19.1 | exact |
| golangci-lint | v2.15.0 (ci.yml, CONTRIBUTING) | 2.15.0 (built with Go 1.27) | 2.15.0 | 2.15.0 | exact |
| gitleaks | 8.30.1 (secrets.yml, CONTRIBUTING) | 8.30.1 | 8.30.1 | 8.30.1 | exact |
| actionlint | none | 1.7.12 | 1.7.12 | not checked | exact |
| shellcheck | none | haskellPackages (not evaluated) | 0.11.0 | not checked | exact |
| gh, jq | none | 2.103.2, 1.8.2 | not checked | not checked | exact |
| bash | 3.2 Mac / 5 CI | 5.3 | 5.3.20 | 5.3.0 | none (see option 2) |
| GNU coreutils / sed / grep / awk | BSD Mac / GNU CI | 9.12 / 4.10 / 3.12 / 5.4.1 | available (g-prefixed) | 9.12.0 / yes | uutils only |

Nothing in nixpkgs lags today; kubectl is one patch ahead (allowed by CONTRIBUTING). Homebrew matches
by timing, not by pin: it serves only its current version. The trial flake's `exactPins` overlay
shows the fallback for helm and kind anyway: `fetchurl` of the upstream release with the Dockerfile's
linux sha256s and the darwin sha256s from the releases' own `.sha256sum` files (the linux ones match
the Dockerfile byte for byte). It is not applied.

## What the sandbox probes showed

- **Nix 2.21.1**: `nix --version` works; `nix store info` fails with `cannot connect to socket at
  '/nix/var/nix/daemon-socket/socket': Operation not permitted`. The socket is `srw-rw-rw-`, so
  this is the sandbox's Unix-socket rule, not permissions. Writes to `/nix/store` are refused too
  (expected; the daemon writes, not the client). `cache.nixos.org` is reachable once allowed by the
  proxy. During the probe the Nix volume was unmounted (procoduck is upgrading to 2.35.4), so no
  `nix develop` timing exists yet; the commands are below. `flake.lock`'s `narHash` was computed with
  a NAR serializer in Python over the unpacked tarball, because `nix hash path` was gone with the
  volume; `nix flake metadata` must leave `flake.lock` unchanged, which confirms it.
- **Docker / OrbStack**: `docker version` fails with `permission denied ... docker.sock`. Same rule.
- **mise 2026.10.23 and pkgx 2.11.0** (single binaries, both at least 3 days old): every download
  fails with `invalid peer certificate: OSStatus -26276`. Both verify TLS through macOS
  Security.framework (rustls-platform-verifier), which the sandbox blocks; Go programs in this
  sandbox only work because of `GODEBUG=x509sslcertoverrideplatform=1`. There is no environment
  switch for the Rust verifier, so inside the sandbox these two can run tools already installed but
  cannot install new ones. Outside the sandbox they work as documented.
- **Homebrew**: the tools already installed run fine; `brew install` writes `/opt/homebrew` and is
  outside the sandbox anyway (a one-time human step).

TMPDIR (#535): GNU `mktemp` honours `TMPDIR`; macOS `mktemp -d` without a template does not, which is
what the sandbox trips on. `nix develop` keeps the caller's `TMPDIR` (it is on its list of variables
it does not import from the derivation), so inside the flake shell a bare `mktemp` would work. That
hides the bug for Nix users only; #535 must still land because scripts must work without Nix.

## Scoring

0 = no, 1 = partly or with a cost, 2 = yes. "Gap" is the bash 3.2 / BSD gap: *fix* (same userland
everywhere) scores 2, *catch* (CI fails on it) scores 1.

| Criterion | 1 Nix flake | 2 mise (+aqua) | 3 pkgx | 4 devcontainer | 5 Brewfile + check | 6 Stay + versions file |
| --- | --- | --- | --- | --- | --- | --- |
| Mac Claude sandbox | 1: daemon socket refused; one sandbox rule fixes it | 1: installs fail (TLS), installed shims run | 1: same as mise | 0: docker socket refused (2 if Claude runs inside the container) | 2: runs installed tools | 2: nothing new |
| Cloud session + CI | 1: install about 10-30 s plus closure, per job | 2: one binary, `jdx/mise-action` | 1: one binary, no lock | 1: image pull per job; cloud has Docker | 0: Linuxbrew is slow and unpinned; CI would not use it | 2: as today |
| Exact pins | 2: lock + overlay | 2: `mise.lock` with checksums | 1: exact if the pantry built it; no checksums in repo | 2: the Dockerfile's sha256s | 0: latest only | 2: as today, one file |
| ADR 0079 cooldown | 2: one revision to age-check | 2: `minimum_release_age` setting | 0: nothing; manual | 2: as today (digest pins) | 0: none | 2: as today |
| bash 3.2 / BSD gap | 2: fix | 0: no bash 5 or GNU sed; coreutils is uutils | 2: fix | 2: fix inside the container | 1: on the Mac, if PATH puts brew first | 1: catch (bash 3.2 job; BSD only on a macOS runner) |
| Contributor setup | 1: install Nix (Determinate installer), optional direnv | 2: one binary | 2: one binary | 1: OrbStack (already needed) + devcontainer CLI or an IDE | 2: `brew bundle` | 2: none |
| Maintenance | 1: bump `flake.lock` monthly, age check, overlay when nixpkgs lags | 2: per tool, Renovate understands mise | 1: small vendor; pantry lag risk | 2: the Dockerfile we already keep | 1: drifts silently; the check script catches it | 1: own a generator and a drift check |
| **Total (of 14)** | **10** | **11** | **8** | **10** | **6** | **12** |

mise scores well on mechanics but fails the reason this issue exists: it cannot give bash 5 or GNU
sed (only an asdf plugin that compiles bash from source), and its coreutils are the Rust rewrite, not
GNU. With aqua alone the answer is the same: aqua installs release binaries, and bash and GNU sed do
not ship any.

## Per option

**1 Nix flake.** `mkShellNoCC` with bash 5.3, GNU coreutils/sed/grep/awk, shellcheck, actionlint,
golangci-lint, gitleaks, `go_1_27`, ko, helm, kind, kubectl, gh, jq for four systems. `go tool` pins
stay in go.mod.
- macOS: the Determinate installer (a single `.pkg` or `curl | sh`, flakes on, a clean uninstaller)
  or upstream's multi-user installer; both create the `/nix` APFS volume and a launchd daemon.
  direnv + nix-direnv gives an automatic, cached shell per checkout. In the Claude Code sandbox the
  client needs the daemon socket: allow `/nix/var/nix/daemon-socket/socket` in the sandbox's Unix
  socket list (a settings change procoduck makes), and set `XDG_CACHE_HOME` to `$TMPDIR` because the
  eval cache lives in `~/.cache/nix`. `/nix/store` reads are already allowed.
- Cloud session: setup script runs the Determinate installer with `--init none` (no systemd in the
  container; single-user store), then `nix develop -c true` to warm the store. Expect a few tens of
  seconds for the install and the closure download (not measured, trial stopped).
- CI: `DeterminateSystems/nix-installer-action` then `nix develop -c <step>`. Everything in this
  shell is on cache.nixos.org (a CDN), so the first cache option is no cache at all; measure before
  adding one. Magic Nix Cache stores paths in the GitHub Actions cache, which Blacksmith replaces
  with its own; whether it works there is untested. Alternatives: cachix (free tier for public
  repositories), FlakeHub Cache (paid), `nix-community/cache-nix-action` (actions/cache).
- Cooldown: the input is one nixpkgs revision. Rule: pin only a `nixos-unstable` channel head that
  was pushed at least 3 days ago (channel heads are the ones Hydra built and cached; a random commit
  on the branch may force local builds). This lock: `31ecbaca1bdefa45dffc409f0250bd8039ea0010`,
  channel push 2026-10-28T02:53Z (commit 2026-10-27), 5 days old today. A check reads `lastModified`
  from `flake.lock` (it is the commit time, a lower bound) and refuses anything younger than 3 days;
  the overlay's fetchurl pins fall under the ADR's per-release rule as today.
- Pins: exact via the lock; the overlay for when nixpkgs lags (not needed today).

**2 mise (+ aqua backend).** `mise.toml` plus `mise.lock` (checksums), `minimum_release_age` for the
cooldown, Renovate support, one static binary for Mac, cloud and CI. Fast and simple, and it would
replace the Dockerfile's download block. It does not touch the bash 3.2 / BSD gap.

**3 pkgx.** One binary; the pantry has bash 5.3, GNU coreutils 9.12 and every pinned tool at the
right version today. But versions resolve against a pantry the project does not control, the
repository holds no checksums or lock, and there is no cooldown mechanism. Small vendor.

**4 devcontainer.** `.devcontainer/devcontainer.json` on an image built from
`hack/e2e-toolchain/Dockerfile` (pinned by digest), so pins and cooldown are what we have today.
Linux userland inside, so the gap is gone for anything run in the container. Costs: on the Mac the
sandbox cannot reach Docker; the Docker-heavy parts (kind, testcontainers, Tilt) need the host's
socket mounted into the container; bind-mounted source is slower on macOS; contributors who run
scripts on the host still hit bash 3.2. CI can run jobs in the image (`container:`), at an image pull
per job.

**5 Brewfile + version check.** `brew bundle` installs bash 5 and GNU tools on the Mac, but Homebrew
has no version pins (only the current formula), no cooldown, and nothing for Linux. A check script
would catch drift; it would not prevent it.

**6 Stay, with one versions file.** `hack/versions.env` (or similar) is the source; a generator
writes the Dockerfile `ARG`s, workflow env and the CONTRIBUTING table, and a CI check fails when they
differ (replacing the hand cross-check in `hack/e2e-remote-test.sh`). Add a job that runs the
`hack/*-test.sh` suite (24 files) under bash 3.2: the `bash:3.2` image on Linux is cheap and catches
bash bugs; only a macOS runner also catches BSD `sed`/`date`. A `hack/doctor.sh` can warn when a
local tool differs from the versions file.

## Nix measurements

Not measured, trial stopped (see Decision): no `nix develop` cold or warm timing, no closure download
size, and no `nix flake metadata` confirmation of the hand-computed `narHash`.

## What an ADR would decide

If option 6 is taken: the versions file's format and location, which files are generated and which
are checked, and the bash 3.2 CI job (container or macOS runner). If Nix is taken later, an ADR
decides: whether the flake is required or optional (optional keeps bash-3.2-safe scripts mandatory),
the nixpkgs channel and the age rule for `flake.lock`, whether CI jobs run in `nix develop`, the
cache, and the sandbox socket rule for agents. Docker and OrbStack stay outside either way: Nix on
macOS cannot provide a Docker engine, and the e2e image stays the Dockerfile.

## Addendum, 2026-11-02: bash 5 required (ADR 0090, #553)

procoduck changed the bash decision after this note: every script now requires bash 5 (Homebrew on macOS,
`#!/usr/bin/env bash` unchanged), with a guard line in each script and a lint. This is the cheaper way
to close the gap the note gave to Nix: option 5's "if PATH puts brew first" became a rule, enforced by
the guard. No bash 3.2 CI job is built, and the Nix flake loses its only unique benefit for bash. What
remains of the "gap" is BSD-versus-GNU tools (sed, grep, date, mktemp), which Homebrew bash does not
change and which the guard does not cover.
