# SPDX-License-Identifier: Apache-2.0
# Spike harness: OpenCode 1.19.3 on the probe base. The compiled opencode binary (glibc
# build from npm) is the process that makes network calls, so policy names it directly.
# dockerfile-lint: allow=digest spike under hack/spike, built by hand for an experiment and never shipped; the base is a moving node tag on purpose in a throwaway fetch stage
FROM docker.io/library/node:24-bookworm-slim AS fetch
RUN cd /tmp && npm pack opencode-linux-x64@1.19.3 >/dev/null && tar xzf opencode-linux-x64-1.19.3.tgz
# dockerfile-lint: allow=digest spike under hack/spike, built by hand for an experiment and never shipped; the probe base is a placeholder registry the builder substitutes
FROM registry.example.invalid/koinon/spike-probe:1
COPY --from=fetch /tmp/package/bin/opencode /usr/local/bin/opencode
COPY opencode.json /etc/koinon/opencode/opencode.json
COPY koinon-opencode /usr/local/bin/koinon-opencode
RUN chmod 0755 /usr/local/bin/koinon-opencode && chmod -R a+rX /etc/koinon
