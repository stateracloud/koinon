# SPDX-License-Identifier: Apache-2.0
# Fixture for hack/ubuntu-snapshot-bump_test.sh: the shape of flavors/opencode/Dockerfile.
FROM ubuntu:24.04
ARG UBUNTU_SNAPSHOT=20261027T000000Z
RUN rm -f /etc/apt/sources.list.d/ubuntu.sources \
 && printf 'Types: deb\nURIs: https://snapshot.ubuntu.com/ubuntu/%s/\nSuites: noble noble-updates noble-security\nComponents: main universe\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' "$UBUNTU_SNAPSHOT" \
      > /etc/apt/sources.list.d/koinon-snapshot.sources \
 && DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends \
      ca-certificates=20260816ubuntu0.24.04.1 \
      git=1:2.43.0-1ubuntu7.4 \
      tree=2.1.1-2ubuntu3.24.04.2 \
      zstd=1.5.5+dfsg2-2build1.1 \
      python-is-python3=3.11.4-1 \
 || { echo "snapshot.ubuntu.com unreachable" >&2; exit 1; } \
 && rm -rf /var/lib/apt/lists/*
RUN echo ca-certificates=20260816ubuntu0.24.04.1 is not a pin here
