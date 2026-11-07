# SPDX-License-Identifier: Apache-2.0
#
# The web variant of the kind e2e toolchain (old issue 358): the toolchain image (Dockerfile here, passed in as
# BASE by digest) plus Node, pnpm and Playwright's Chromium with its system packages, so
# `SUITE=web test/e2e/run.sh` (make test-e2e-web) needs neither the Playwright CDN nor apt at run
# time; only the npm registry for `pnpm install`. Published to ghcr.io/stateracloud/koinon/e2e-toolchain-web
# by .github/workflows/e2e-toolchain.yml.
#
# Keep the versions in step with the repository: NODE_VERSION's major with actions/setup-node in
# ci.yml, PNPM_VERSION with web/package.json's packageManager, PLAYWRIGHT_VERSION with its
# @playwright/test (hack/e2e-remote-test.sh checks all three). A Playwright bump in web/ without one
# here makes run.sh download the new browser, which a locked-down egress refuses.
ARG BASE
FROM ${BASE}
ARG TARGETARCH
ARG NODE_VERSION=24.22.2
ARG NODE_SHA256_amd64=48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75
ARG NODE_SHA256_arm64=eb187837a2230d4047161ac0be008584e16b46f726e9c9490ee35dc6b17d9ba8
ARG PNPM_VERSION=12.14.0
ARG PLAYWRIGHT_VERSION=1.64.1

# Browsers in a fixed place the run finds through the environment, not in a user's cache.
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    node_arch=x64; [ "$arch" = arm64 ] && node_arch=arm64; \
    sum=$(eval "echo \$NODE_SHA256_$arch"); \
    curl -fsSL --retry 3 -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"; \
    echo "$sum  /tmp/node.tar.xz" | sha256sum -c -; \
    apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update; apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y --no-install-recommends xz-utils; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md'; \
    rm -f /tmp/node.tar.xz; \
    npm install -g --no-fund --no-audit "pnpm@${PNPM_VERSION}"; \
    printf 'Acquire::Retries "5";\nAcquire::http::Timeout "30";\nAPT::Update::Error-Mode "any";\n' > /etc/apt/apt.conf.d/80-koinon-retries; \
    npx -y "playwright@${PLAYWRIGHT_VERSION}" install --with-deps chromium; \
    rm -rf /var/lib/apt/lists/* /root/.npm; \
    node --version; pnpm --version
