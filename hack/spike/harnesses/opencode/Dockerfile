# SPDX-License-Identifier: Apache-2.0
# Spike harness: OpenCode 1.19.3 on the probe base. The compiled opencode binary (glibc
# build from npm) is the process that makes network calls, so policy names it directly.
FROM docker.io/library/node:24-bookworm-slim AS fetch
RUN cd /tmp && npm pack opencode-linux-x64@1.19.3 >/dev/null && tar xzf opencode-linux-x64-1.19.3.tgz
FROM registry.example.invalid/koinon/spike-probe:1
COPY --from=fetch /tmp/package/bin/opencode /usr/local/bin/opencode
COPY opencode.json /etc/koinon/opencode/opencode.json
COPY koinon-opencode /usr/local/bin/koinon-opencode
RUN chmod 0755 /usr/local/bin/koinon-opencode && chmod -R a+rX /etc/koinon
