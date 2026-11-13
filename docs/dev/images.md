<!-- SPDX-License-Identifier: Apache-2.0 -->

# Images: the Dockerfile pattern for a new non-Go image

- Status: Current
- Area: engineering-process
- Tags: harness, dependencies

Koinon builds its Go images with `ko`. Every other image (a harness image, a toolchain image) is a
Dockerfile, and [`harnesses/opencode/Dockerfile`](../../harnesses/opencode/Dockerfile) is the one to
copy (`flavors/` is a symlink to `harnesses/`, #610). This page lists the five rules that Dockerfile
follows, why each exists and the lines that carry it, so that you can write a new image without
reading the whole file. Railpack was considered and rejected; Dockerfiles stay.

## Use this page when

You add an image that is not built by `ko`: a new harness (`harnesses/<name>/`), a CI toolchain
image (compare `hack/e2e-toolchain/Dockerfile`) or a change to the OpenCode image. For a Go
binary, add it to the ko build instead.

The quotes below are copied from `harnesses/opencode/Dockerfile` and
`harnesses/common/koinon-fetch` as they stand. Line numbers drift, so search for the quoted text.
Versions, digests and the `...` gaps are the only things that change between images.

## At a glance

| Rule | Why | Where in the Dockerfile |
| --- | --- | --- |
| Download per architecture, check a sha256 | Same bytes on `amd64` and `arm64`, on every build | `fetch-amd64`, `fetch-arm64`, `fetch-tools-*` stages |
| One download step, secret mounted there only | A token must not reach a layer, a log or the environment | `RUN --mount=type=secret,...` and `koinon-fetch` |
| apt from `snapshot.ubuntu.com`, with retries | Same packages on any day; an outage must not fail a build silently | `UBUNTU_SNAPSHOT`, `Acquire::Retries=5` |
| A licences stage | Redistributed binaries ship their licence texts | `FROM scratch AS licenses` |
| A non-root user | OpenShell runs the sandbox as UID/GID 10001 | `groupadd` / `useradd`, `USER 10001:10001` |

Pin the base too: the file starts with a `# syntax=` line carrying a digest and takes the base from
one `ARG BASE` that carries its digest.

```dockerfile
# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686
```

`hack/harness-base.sh` lets a mirror replace the base, but only with the same digest, so the base
never changes without a change to the Dockerfile.

## 1. Per-architecture downloads, each checked against a checksum

**Why.** The image is built for `linux/amd64` and `linux/arm64` (`make harness-opencode`). A
download whose bytes are not pinned changes under you without a diff, and a "latest" URL makes
two builds of one commit differ. Dependencies also pass the three-day cooldown of
[ADR 0079](../adr/0079-dependency-cooldown.md), which only works when the version is pinned in the
tree. A separate stage per architecture, selected by `TARGETARCH`, puts each architecture's URL and
digest next to each other, so a re-pin is one diff and neither platform can take the other's file.

**The lines.** BuildKit's `ADD --checksum` downloads and verifies in one instruction, and the build
fails on a mismatch. The stage is then chosen with `${TARGETARCH}`, and the unpacked binary is
checked with `--version`, so a mislabelled archive fails the build:

```dockerfile
FROM ${BASE} AS fetch-amd64
ADD --checksum=sha256:dfcd69f1d511ae10ed626768b41d84b8100cc4aa64cecb69da38ba7a29d33551 \
    https://registry.npmjs.org/@opencode/cli-linux-x64/-/cli-linux-x64-2.2.6.tgz /tmp/opencode.tgz

FROM ${BASE} AS fetch-arm64
ADD --checksum=sha256:22352e200630ec61a48a65b9f5c2b627328ae5002658d81c067f67c5fb689898 \
    https://registry.npmjs.org/@opencode/cli-linux-arm64/-/cli-linux-arm64-2.2.6.tgz /tmp/opencode.tgz

FROM fetch-${TARGETARCH} AS fetch
RUN tar -xzf /tmp/opencode.tgz -C /tmp package/bin/opencode \
 && install -m 0755 /tmp/package/bin/opencode /opencode \
 && /opencode --version | grep -qx 'opencode v2.2.6'
```

The comment above it records where each digest came from (here: npm's sha512 integrity, checked
before the sha256 was computed). Do the same, so a reviewer can repeat the check. A tool from a
GitHub release gets the same shape, but the download is the next rule's `koinon-fetch`, and the
final image takes only the files it needs:

```dockerfile
FROM fetch-tools-${TARGETARCH} AS tools
RUN set -eux; \
...
    fd_v=$(/out/bin/fd --version); [ "$fd_v" = 'fd 10.5.0' ] || { echo "fd: $fd_v"; exit 1; }; \
    yq_v=$(/out/bin/yq --version); case $yq_v in *' version v4.54.1') ;; *) echo "yq: $yq_v"; exit 1 ;; esac; \
    rg_v=$(/out/bin/rg --version | head -1); case $rg_v in 'ripgrep 15.2.0'*) ;; *) echo "rg: $rg_v"; exit 1 ;; esac; \
    /out/libexec/duckdb -c 'SELECT version()' | grep -q 'v2\.0\.0'
```

## 2. One download step, with the BuildKit secret mounted (never an env var)

**Why.** Anonymous GitHub release downloads are rate limited per runner IP and sometimes return a
fixed error body, which fails a pinned checksum (#145). An authenticated request has its own
limit, so CI passes its token. A token in an `ARG` or `ENV` is stored in the image history and
visible in `docker inspect`; a token in a `RUN` environment can leak into a log. This is the
"no secrets in sandboxes" rule of [AGENTS.md](../../AGENTS.md) (credentials never in sandbox env,
images, home volumes or logs; the only exception is [ADR 0044](../adr/0044-in-sandbox-credential-exception.md)),
applied to the build. The secret is optional (`required=false`), so a local or fork build works
anonymously.

**The lines.** The header comment states the contract; each download is one `RUN` that mounts the
secret for that instruction only, and nothing else in the file sees it:

```dockerfile
# GitHub release downloads use the optional BuildKit secret github_token (--secret id=github_token,
# env=GITHUB_TOKEN; CI passes its token, #145). Without it they are anonymous. It is never an ARG
# or ENV and no layer keeps it.
...
FROM dl AS fetch-tools-amd64
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c \
    https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/ripgrep-15.2.0-x86_64-unknown-linux-musl.tar.gz /tmp/rg.tgz
```

The Makefile passes it from the environment of the build host, never as a build argument:

```make
harness_secret     = $(if $(GITHUB_TOKEN),--secret id=github_token$(comma)env=GITHUB_TOKEN)
```

`harnesses/common/koinon-fetch <sha256> <url> <dest>` (the build context `common`, copied in by
`COPY --from=common --chmod=0755 koinon-fetch /usr/local/bin/koinon-fetch`) is the one downloader.
It reads the secret file, keeps the token out of argv, the environment and the log, and checks the
sha256 last:

```sh
token_file=${KOINON_FETCH_TOKEN_FILE:-/run/secrets/github_token}
...
  printf 'header = "Authorization: Bearer %s"\n' "$token" | curl -fsSL --retry 3 --retry-all-errors -K - "$@"
...
echo "$sha  $dest" | sha256sum -c -
```

Use `koinon-fetch` for every GitHub release asset instead of a bare `curl` or an `ADD` of a release
URL: `hack/koinon-fetch-test.sh` fails a new `ADD` of a release asset, and `hack/pinned-download-lint.sh`
fails a bare `curl` or `wget` in scripts. Details: [docs/dev/ci.md](ci.md#image-builds-authenticated-downloads-and-layer-cache-145).

## 3. apt from snapshot.ubuntu.com, with retries

**Why.** Every apt package comes from one Ubuntu snapshot at an exact version, so a build on any day
gets the same packages and a security update is a deliberate bump (`hack/ubuntu-snapshot-bump.sh`,
three days old by construction, [ADR 0079](../adr/0079-dependency-cooldown.md)). The snapshot
service answers 5xx now and then, so every apt call retries and times out (#149), and every
`update` fails the build at once (`APT::Update::Error-Mode=any`, #141; without it a failed fetch only
warns and the build fails later with "Unable to locate package"). `hack/apt-retries-test.sh` fails
any `apt-get` without these options.

**The lines.** The snapshot is an `ARG` and a deb822 source written into the image. `apt --snapshot`
is not used: it leaves the arm64 mirror unchanged, and the snapshot service serves both
architectures under `/ubuntu/`. The update step prints one line that names the snapshot when it
fails, and each pin is `name=version`:

```dockerfile
ARG UBUNTU_SNAPSHOT=20261027T000000Z
RUN rm -f /etc/apt/sources.list.d/ubuntu.sources \
 && printf 'Types: deb\nURIs: https://snapshot.ubuntu.com/ubuntu/%s/\nSuites: noble noble-updates noble-security\nComponents: main universe\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' "$UBUNTU_SNAPSHOT" \
      > /etc/apt/sources.list.d/koinon-snapshot.sources \
 && { apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
      || { echo "package index update from snapshot.ubuntu.com (snapshot ${UBUNTU_SNAPSHOT}) failed after retries: most likely the snapshot mirror is unreachable; otherwise check UBUNTU_SNAPSHOT and the archive keyring" >&2; exit 1; }; } \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates=20260816ubuntu0.24.04.1 \
      git=1:2.43.0-1ubuntu7.4 \
...
 && rm -rf /var/lib/apt/lists/* \
      /var/log/apt/* /var/log/dpkg.log /var/log/alternatives.log \
      /var/cache/ldconfig/aux-cache /var/cache/debconf/*-old
```

The base image has no CA bundle, which the snapshot's HTTPS needs. A throwaway stage fetches one from
the archive over HTTP (apt checks it against the archive keyring), and the final image copies only
that file; the pinned `ca-certificates` then regenerates it:

```dockerfile
FROM ${BASE} AS ca-bootstrap
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
...
COPY --from=ca-bootstrap /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
```

Stages that never reach the final image (`dl`, `ca-bootstrap`) still carry the retry options
(lines 49 to 51), because the checker applies to every apt call. A step that runs apt through another
tool writes an `apt.conf.d` snippet with the same settings instead (see `hack/e2e-toolchain/web.Dockerfile`).

## 4. A licences stage

**Why.** An image that redistributes a binary must ship its licence text. Some release archives carry
none (yq, ast-grep and DuckDB here), so the stage fetches the text from the tagged sources, pinned by
checksum like any other download. Licence files that ship in a tarball are installed next to the
tool. A `FROM scratch` stage holds only those files, which the final image copies by name.

**The lines.** The stage, then the `COPY` into the final image under `/usr/share/doc/koinon-tools/`:

```dockerfile
# The tarballs of yq, ast-grep and DuckDB carry no licence file, so the MIT texts come from the
# tagged sources (sha256 of the files fetched on 2026-10-28). fd's tarball has its own.
FROM scratch AS licenses
ADD --checksum=sha256:697db34dabb21562fe84487a2ccd031fbd45382b89c2cbdec8ef31682c486040 https://raw.githubusercontent.com/mikefarah/yq/v4.54.1/LICENSE /yq
ADD --checksum=sha256:81471889c77b2161a3e4dcdb1b2e6ca382e485766132d92d5fe1d7497e7dd2d9 https://raw.githubusercontent.com/ast-grep/ast-grep/0.51.0/LICENSE /ast-grep
ADD --checksum=sha256:075c33400ffcb0c586dd106a029d3e733e9da3693f8fcb9cebfc651c8a2f14e3 https://raw.githubusercontent.com/duckdb/duckdb/v2.0.0/LICENSE /duckdb
...
COPY --from=licenses /yq /usr/share/doc/koinon-tools/yq/LICENSE
COPY --from=licenses /ast-grep /usr/share/doc/koinon-tools/ast-grep/LICENSE
COPY --from=licenses /duckdb /usr/share/doc/koinon-tools/duckdb/LICENSE
```

Licence files that ship in the tarballs are installed in the build stage and copied the same way:

```dockerfile
    install -m 0644 /x/fd/LICENSE-MIT /x/fd/LICENSE-APACHE /out/doc/fd/; \
...
COPY --from=tools /out/doc/fd/ /usr/share/doc/koinon-tools/fd/
COPY --from=tools /out/doc/rg/ /usr/share/doc/koinon-tools/ripgrep/
```

Record the licence of each tool in the image's README (`harnesses/opencode/README.md` has the
table) and note any GPL package that ships unmodified from Ubuntu.

## 5. A non-root user

**Why.** OpenShell runs the sandbox as UID/GID 10001 with the home volume at `/sandbox` (spike 1, 8;
[ADR 0048](../adr/0048-openshell-spike-results.md)), and the hardening defaults of
[ADR 0045](../adr/0045-openshell-hardening-defaults.md) must not be weakened. So the image runs
as that user, the files are root-owned and not writable by it, and only `/sandbox` belongs to the user.

**The lines.** The user and its home, created with fixed IDs:

```dockerfile
ARG SOURCE_DATE_EPOCH
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8).
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox
```

`SOURCE_DATE_EPOCH` is declared after the apt layer on purpose (#176): `useradd` writes it into
`/etc/shadow` instead of the current day, and a declared `ARG` is part of the cache key of every
later `RUN`, so an earlier declaration would make a build with another epoch miss the cached apt
install. The end of the file makes configuration and libraries read-only, drops root and sets the
working directory:

```dockerfile
RUN chmod -R a+rX,go-w /etc/koinon /usr/local/lib/koinon
USER 10001:10001
WORKDIR /sandbox
```

## Checklist for a new image

1. Copy `harnesses/opencode/Dockerfile`, keep the `# syntax=` digest and the digest-pinned `ARG BASE`.
2. One fetch stage per architecture, `ADD --checksum` or `koinon-fetch`, a `--version` check, and a
   comment that says how each digest was obtained.
3. Mount `github_token` with `required=false` in the download `RUN` only, never `ARG` or `ENV`.
4. apt from one `UBUNTU_SNAPSHOT`, exact `name=version` pins, the retry options on every call,
   `rm -rf /var/lib/apt/lists/*`; run `hack/apt-retries-test.sh`.
5. A `licenses` stage and the files copied under `/usr/share/doc/`.
6. UID/GID 10001, root-owned read-only files, `USER 10001:10001`, no Koinon binary in the image
   ([ADR 0049](../adr/0049-v01-scope-decisions.md)).
7. A Makefile target like `harness-opencode`, an entry in `HARNESSES` in the Makefile and the harness seam described in
   [harnesses/README.md](../../harnesses/README.md), and a run of `make test-harnesses` (Docker).
8. Tell the chart: a sandbox image reference goes through the registry rewrite of
   [ADR 0061](../adr/0061-image-pull-secrets-and-registry-mirrors.md) (`koinon.imageRef`), so do not
   hard-code a registry host in a template.
