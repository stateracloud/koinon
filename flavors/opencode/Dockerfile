# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon OpenCode flavor (ADR 0035, 0048, 0052): OpenCode driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-opencode (docker buildx, linux/amd64 and linux/arm64).
# GitHub release downloads use the optional BuildKit secret github_token (--secret id=github_token,
# env=GITHUB_TOKEN; CI passes its token, #145). Without it they are anonymous. It is never an ARG
# or ENV and no layer keeps it.

# OpenShell's default sandbox base (ADR 0035), multi-arch index digest, checked 2026-10-22.
ARG BASE=nvcr.io/nvidia/base/ubuntu:24.04@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686

# OpenCode 2.2.0 (ADR 0052) is the compiled glibc build that npm's @opencode/cli package installs
# as an optional dependency (@opencode/cli-linux-<arch>). The sha256 values below were computed
# from the registry tarballs after checking them against npm's published sha512 integrity:
#   x64:   sha512-XTGFTiZTgugps6rgTXp4mPjOM8klsa5uHn/Shcms2o3k1bjSA3Sgl6O4yJ48y18WWxn9My5Pxt7JKcLID7LOHg==
#   arm64: sha512-NCCltb8h/8WL30X9o2KTm3UFX0lmfk48lG8tglXJN9moscxeB7w2qOmfHVa32iFngk0iVAKH4xRPlBc88Y0qlw==
# Re-pinning is issue koinon-private#54: the entry script and README name 2.2.0-specific workarounds.
FROM ${BASE} AS fetch-amd64
ADD --checksum=sha256:fe3046b86872e624f62268058f4801a1f1efffc193103a9988238c891b8ca028 \
    https://registry.npmjs.org/@opencode/cli-linux-x64/-/cli-linux-x64-2.2.0.tgz /tmp/opencode.tgz

FROM ${BASE} AS fetch-arm64
ADD --checksum=sha256:6a40643c0dca3aff9d37c87c26c0ee003c46abb810e1c6ddde094ffbf7460b8b \
    https://registry.npmjs.org/@opencode/cli-linux-arm64/-/cli-linux-arm64-2.2.0.tgz /tmp/opencode.tgz

FROM fetch-${TARGETARCH} AS fetch
RUN tar -xzf /tmp/opencode.tgz -C /tmp package/bin/opencode \
 && install -m 0755 /tmp/package/bin/opencode /opencode \
 && /opencode --version | grep -qx 'opencode v2.2.0'

# Data and code tools for the agent (ADR 0052, amendment 2026-10-28): small release binaries, one
# fetch stage per architecture like OpenCode's. Each sha256 is the GitHub release asset's `digest`
# (release API, checked 2026-10-28), and the assets were also hashed locally; yq's value matches
# the SHA-256 column of the release's `checksums` file. fd, ast-grep and DuckDB publish no
# checksum file. Re-pinning means new URLs, new digests and the `--version` checks below.
#   ripgrep 15.2.0    Unlicense OR MIT   static musl build (upstream .sha256 files agree)
#   fd 10.5.0         MIT OR Apache-2.0  static musl build
#   yq 4.54.1         MIT                mikefarah/yq, static Go build
#   ast-grep 0.51.0   MIT                glibc build; `sg` is left out, it would shadow /usr/bin/sg
#   DuckDB CLI 2.0.0  MIT                glibc build; reached through the koinon-duckdb wrapper
# The downloader for the GitHub release assets below (flavors/common/koinon-fetch): curl with the
# build's optional github_token secret (#145), then a sha256sum check against the pin. The base has
# no curl or CA bundle; like ca-bootstrap this stage takes both from the archive over HTTP, and it
# never reaches the final image, so its apt state does not matter to the result.
FROM ${BASE} AS dl
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=common --chmod=0755 koinon-fetch /usr/local/bin/koinon-fetch

FROM dl AS fetch-tools-amd64
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c \
    https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/ripgrep-15.2.0-x86_64-unknown-linux-musl.tar.gz /tmp/rg.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 761c72dc8e120d85b22292063be8a796e2eeb20eb3e4f38b8fa2343ccf3514a7 \
    https://github.com/sharkdp/fd/releases/download/v10.5.0/fd-v10.5.0-x86_64-unknown-linux-musl.tar.gz /tmp/fd.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch e68a456f90c577af3fe4960184b3a3cf5c461e0348407c10f107da3a5fec8972 \
    https://github.com/mikefarah/yq/releases/download/v4.54.1/yq_linux_amd64.tar.gz /tmp/yq.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch a7eead5552d3a8daad70f5d50d935da44bd0c138e946b5b0a96b1d38169fa639 \
    https://github.com/ast-grep/ast-grep/releases/download/0.51.0/app-x86_64-unknown-linux-gnu.zip /tmp/ast-grep.zip
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch c6507206baf7793c6013dbd82368469a45f20dc0e17a6f92f204602f652ed2bc \
    https://github.com/duckdb/duckdb/releases/download/v2.0.0/duckdb-cli-linux-amd64.tar.gz /tmp/duckdb.tgz

FROM dl AS fetch-tools-arm64
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915 \
    https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/ripgrep-15.2.0-aarch64-unknown-linux-musl.tar.gz /tmp/rg.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch d76c4317f7d5dba69f8a2a15856c90c777e7f0dd4e85f0de8c76de6992c374d4 \
    https://github.com/sharkdp/fd/releases/download/v10.5.0/fd-v10.5.0-aarch64-unknown-linux-musl.tar.gz /tmp/fd.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 8c27f6e7476fb5acefb9352e3d30f6bc2dc93f1956eb92e8da727173fb7ec6d3 \
    https://github.com/mikefarah/yq/releases/download/v4.54.1/yq_linux_arm64.tar.gz /tmp/yq.tgz
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 73af0ebe5958acee012754a5b2826a186580ef83333092408a7a01096fc30b8b \
    https://github.com/ast-grep/ast-grep/releases/download/0.51.0/app-aarch64-unknown-linux-gnu.zip /tmp/ast-grep.zip
RUN --mount=type=secret,id=github_token,required=false \
    koinon-fetch 859e1875d2a9515fbb4bf8871061437a14af82320da68c753f094c4b776348fe \
    https://github.com/duckdb/duckdb/releases/download/v2.0.0/duckdb-cli-linux-arm64.tar.gz /tmp/duckdb.tgz

# The tarballs of yq, ast-grep and DuckDB carry no licence file, so the MIT texts come from the
# tagged sources (sha256 of the files fetched on 2026-10-28). fd's tarball has its own.
FROM scratch AS licenses
ADD --checksum=sha256:697db34dabb21562fe84487a2ccd031fbd45382b89c2cbdec8ef31682c486040 https://raw.githubusercontent.com/mikefarah/yq/v4.54.1/LICENSE /yq
ADD --checksum=sha256:81471889c77b2161a3e4dcdb1b2e6ca382e485766132d92d5fe1d7497e7dd2d9 https://raw.githubusercontent.com/ast-grep/ast-grep/0.51.0/LICENSE /ast-grep
ADD --checksum=sha256:075c33400ffcb0c586dd106a029d3e733e9da3693f8fcb9cebfc651c8a2f14e3 https://raw.githubusercontent.com/duckdb/duckdb/v2.0.0/LICENSE /duckdb

FROM fetch-tools-${TARGETARCH} AS tools
RUN set -eux; \
    mkdir -p /x/rg /x/fd /x/yq /x/duckdb /out/bin /out/libexec /out/doc/fd /out/doc/rg; \
    tar -xzf /tmp/rg.tgz -C /x/rg --strip-components=1; \
    tar -xzf /tmp/fd.tgz -C /x/fd --strip-components=1; \
    tar -xzf /tmp/yq.tgz -C /x/yq; \
    tar -xzf /tmp/duckdb.tgz -C /x/duckdb; \
    install -m 0755 /x/fd/fd /out/bin/fd; \
    install -m 0644 /x/fd/LICENSE-MIT /x/fd/LICENSE-APACHE /out/doc/fd/; \
    install -m 0755 /x/yq/yq_linux_* /out/bin/yq; \
    install -m 0755 /x/rg/rg /out/bin/rg; \
    install -m 0644 /x/rg/UNLICENSE /x/rg/LICENSE-MIT /x/rg/COPYING /out/doc/rg/; \
    cp /tmp/ast-grep.zip /out/ast-grep.zip; \
    install -m 0755 /x/duckdb/duckdb /out/libexec/duckdb; \
    fd_v=$(/out/bin/fd --version); [ "$fd_v" = 'fd 10.5.0' ] || { echo "fd: $fd_v"; exit 1; }; \
    yq_v=$(/out/bin/yq --version); case $yq_v in *' version v4.54.1') ;; *) echo "yq: $yq_v"; exit 1 ;; esac; \
    rg_v=$(/out/bin/rg --version | head -1); case $rg_v in 'ripgrep 15.2.0'*) ;; *) echo "rg: $rg_v"; exit 1 ;; esac; \
    /out/libexec/duckdb -c 'SELECT version()' | grep -q 'v2\.0\.0'

# Every apt package comes from one Ubuntu snapshot (snapshot.ubuntu.com), so a build on any day
# gets the same versions; the base is pinned by digest. Bump UBUNTU_SNAPSHOT on purpose to take
# security updates, and the versions below with it (apt-cache policy shows the candidate).
# apt's own --snapshot flag is not used: it leaves the arm64 mirror (ports.ubuntu.com), which has
# no snapshots, unchanged. The snapshot service serves both architectures under /ubuntu/.
# The base has no CA bundle, which the snapshot's HTTPS needs: this stage borrows one from the
# archive over HTTP (apt checks it against the archive keyring) and the final image takes only
# the bundle file, which the pinned ca-certificates below then regenerates.
FROM ${BASE} AS ca-bootstrap
RUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

FROM ${BASE}
ARG UBUNTU_SNAPSHOT=20261027T000000Z
COPY --from=ca-bootstrap /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# git for the repository the agent works on, ca-certificates for TLS through the OpenShell proxy. The
# data tools: jq, sqlite3, poppler-utils (pdftotext, pdfinfo; GPL, shipped unmodified from Ubuntu
# like git and bash), file, tree, unzip, zstd and xz-utils; python3 is the standard library only
# (python-is-python3 adds `python`). No pip: the packages that carry it (python3-pip,
# python3-venv) are not installed and --no-install-recommends keeps it that way. Components:
# everything is main except tree (universe). Versions are the snapshot's candidates.
RUN rm -f /etc/apt/sources.list.d/ubuntu.sources \
 && printf 'Types: deb\nURIs: https://snapshot.ubuntu.com/ubuntu/%s/\nSuites: noble noble-updates noble-security\nComponents: main universe\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' "$UBUNTU_SNAPSHOT" \
      > /etc/apt/sources.list.d/koinon-snapshot.sources \
 && { apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \
      || { echo "package index update from snapshot.ubuntu.com (snapshot ${UBUNTU_SNAPSHOT}) failed after retries: most likely the snapshot mirror is unreachable; otherwise check UBUNTU_SNAPSHOT and the archive keyring" >&2; exit 1; }; } \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates=20260816ubuntu0.24.04.1 \
      git=1:2.43.0-1ubuntu7.4 \
      jq=1.7.1-3ubuntu0.24.04.2 \
      sqlite3=3.45.1-1ubuntu2.8 \
      poppler-utils=24.02.0-1ubuntu9.11 \
      file=1:5.45-3build1 \
      tree=2.1.1-2ubuntu3.24.04.2 \
      unzip=6.0-28ubuntu4.1 \
      zstd=1.5.5+dfsg2-2build1.1 \
      xz-utils=5.6.1+really5.4.5-1ubuntu0.4 \
      python3=3.12.3-0ubuntu2.1 \
      python-is-python3=3.11.4-1 \
 && rm -rf /var/lib/apt/lists/*
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8).
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox
COPY --from=fetch /opencode /usr/local/bin/opencode
# The data tools (fetch-tools stages above). DuckDB's binary sits out of PATH; /usr/local/bin/duckdb
# is the wrapper that applies /etc/koinon/duckdb/init.sql.
COPY --from=tools /out/bin/ /usr/local/bin/
COPY --from=tools /out/libexec/duckdb /usr/local/libexec/duckdb
COPY --chmod=0755 koinon-duckdb /usr/local/bin/duckdb
COPY --chmod=0644 etc/koinon/duckdb/init.sql /etc/koinon/duckdb/init.sql
COPY --from=tools /out/doc/fd/ /usr/share/doc/koinon-tools/fd/
COPY --from=tools /out/doc/rg/ /usr/share/doc/koinon-tools/ripgrep/
COPY --from=tools /out/ast-grep.zip /tmp/ast-grep.zip
# ast-grep ships as a zip; unzip is the snapshot's pinned one. Its exact version is checked below.
RUN unzip -q -p /tmp/ast-grep.zip ast-grep > /usr/local/bin/ast-grep \
 && chmod 0755 /usr/local/bin/ast-grep \
 && rm /tmp/ast-grep.zip
COPY --from=licenses /yq /usr/share/doc/koinon-tools/yq/LICENSE
COPY --from=licenses /ast-grep /usr/share/doc/koinon-tools/ast-grep/LICENSE
COPY --from=licenses /duckdb /usr/share/doc/koinon-tools/duckdb/LICENSE
# Build-time check, as root: fail the image if the standard library the agent relies on is
# incomplete, if pip came in with a dependency, or if a tool or the DuckDB defaults do not work.
# RUN uses dash, which has no pipefail: `tool | head -1` would pass with the tool missing (head
# exits 0), so unzip and xz run in a command substitution, where `set -e` sees their status (#190).
RUN set -eux; \
    python3 -I -c 'import json, csv, sqlite3, gzip, zipfile, statistics, decimal, datetime, re, subprocess, shutil, tarfile'; \
    ! command -v pip || exit 1; ! command -v pip3 || exit 1; ! command -v ensurepip || exit 1; \
    ! python3 -I -c 'import ensurepip' 2>/dev/null; \
    ! python3 -I -m pip --version 2>/dev/null; \
    ! command -v uv ruff || exit 1; \
    python --version; \
    jq --version; sqlite3 --version; pdftotext -v; pdfinfo -v; file --version; tree --version; \
    unzip_v=$(unzip -v); echo "$unzip_v" | head -1; zstd --version; xz_v=$(xz --version); echo "$xz_v" | head -1; \
    fd --version; yq --version; \
    rg --version | head -1 | grep -q '^ripgrep 15\.2\.0'; test "$(ast-grep --version)" = 'ast-grep 0.51.0'; \
    duckdb -c "SELECT version()"; \
    test "$(duckdb -noheader -list -c "SELECT current_setting('memory_limit')")" = '768.0 MiB'; \
    test "$(duckdb -noheader -list -c "SELECT current_setting('threads')")" = 2; \
    test "$(duckdb -noheader -list -c "SELECT current_setting('autoinstall_known_extensions')")" = false; \
    duckdb -c "COPY (SELECT 1 AS a) TO '/tmp/koinon-check.parquet' (FORMAT parquet); SELECT * FROM '/tmp/koinon-check.parquet'; SELECT '{\"a\":1}'::JSON;"; \
    rm -rf /tmp/koinon-check.parquet /tmp/duckdb
# The entry scripts' shared helpers (flavors/common, build context "common").
COPY --from=common --chmod=0755 koinon-entry-lib.sh /usr/local/lib/koinon/koinon-entry-lib.sh
COPY --chmod=0755 koinon-opencode /usr/local/bin/koinon-opencode
COPY --chmod=0644 etc/koinon/opencode/opencode.jsonc /etc/koinon/opencode/opencode.jsonc
# Koinon's retry policy for OpenCode's model requests, loaded by the config's plugin list
# (README.md, "Model retries"); outside every config root, so nothing else loads it.
COPY --chmod=0644 plugins/koinon-retry/index.js /usr/local/lib/koinon/opencode/plugins/koinon-retry/index.js
# git's GitHub credential helper, shared by every flavor (flavors/common, build context
# "common"): it prints the OpenShell placeholder and stores nothing.
COPY --from=common --chmod=0755 koinon-git-credential /usr/local/bin/koinon-git-credential
COPY --from=common --chmod=0644 etc/koinon/git/config /etc/koinon/git/config
RUN printf '[include]\n\tpath = /etc/koinon/git/config\n\tpath = /sandbox/.koinon/git/config\n' >> /etc/gitconfig
# Koinon's instructions are per run: koinon-agentd writes /sandbox/.koinon/AGENTS.md and the
# config's "instructions" key names it (see README.md).
RUN chmod -R a+rX,go-w /etc/koinon /usr/local/lib/koinon
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-opencode"]
