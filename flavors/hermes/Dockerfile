# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Koinon Hermes flavor (ADR 0035, 0048): Hermes Agent driven over ACP by koinon-agentd through
# OpenShell's non-TTY exec. No Koinon binary is in this image (ADR 0049).
#
# Build: make flavor-hermes (docker buildx, linux/amd64 and linux/arm64).

# The official Hermes Agent image, v0.22.3 (commit 5b147ab036d6d19131bff8d20701b5001eab844a,
# from its install-stamp.json), multi-arch index digest, checked 2026-10-22. It carries Hermes
# with its Python 3.14 virtual environment in /opt/hermes, git and ripgrep, on Debian 13. Its
# s6 supervisor and entrypoint are not used: OpenShell runs the sandbox's own process, and
# koinon-agentd execs koinon-hermes.
FROM docker.io/nousresearch/hermes-agent:v0.22.3@sha256:2f305c9bdc81731f48664586d23cb6a6f838a15ad7be1bfabe0c3c269d8e09b9
USER root
# OpenShell runs the sandbox as UID/GID 10001 with the home volume at /sandbox (spike 1, 8);
# the image's own hermes user (10000) is not used.
RUN groupadd --gid 10001 sandbox \
 && useradd --uid 10001 --gid 10001 --home-dir /sandbox --no-create-home --shell /bin/bash sandbox \
 && install -d -o 10001 -g 10001 -m 0750 /sandbox \
 && /opt/hermes/.venv/bin/hermes --version | grep -q 'v0\.22\.3'
# The entry scripts' shared helpers (flavors/common, build context "common").
COPY --from=common --chmod=0755 koinon-entry-lib.sh /usr/local/lib/koinon/koinon-entry-lib.sh
COPY --chmod=0755 koinon-hermes /usr/local/bin/koinon-hermes
COPY --chmod=0644 etc/koinon/hermes/config.yaml /etc/koinon/hermes/config.yaml
# git's GitHub credential helper, shared by every flavor (flavors/common, build context
# "common"): it prints the OpenShell placeholder and stores nothing.
COPY --from=common --chmod=0755 koinon-git-credential /usr/local/bin/koinon-git-credential
COPY --from=common --chmod=0644 etc/koinon/git/config /etc/koinon/git/config
RUN /usr/bin/git --version \
 && printf '[include]\n\tpath = /etc/koinon/git/config\n' >> /etc/gitconfig
# Hermes's managed scope: read-only, outside the home, and over the user config at every key
# it sets (README.md).
COPY --chmod=0644 etc/hermes/config.yaml /etc/hermes/config.yaml
# /opt/hermes is already world-readable; the read-only root file system keeps it unchanged.
RUN chmod -R a+rX,go-w /etc/koinon /etc/hermes
# The managed scope must parse with Hermes's loader and pin every shell hook event this Hermes
# knows, and Python must be the interpreter the reconciler allows to reach agentgateway
# (internal/reconciler, DefaultBinaries): OpenShell matches the real path of the executable.
RUN /opt/hermes/.venv/bin/python -I -B -c 'import sys; sys.path.insert(0, "/opt/hermes"); \
from hermes_cli import managed_scope as m; from hermes_cli.plugins import SHELL_UNSUPPORTED_HOOKS, VALID_HOOKS; \
c = m.load_managed_config(); hooks = c.get("hooks") or {}; \
missing = sorted(set(VALID_HOOKS) - set(SHELL_UNSUPPORTED_HOOKS) - set(hooks)); \
assert c.get("platform_toolsets", {}).get("acp"), "managed scope not read"; \
assert not missing, "hook events not pinned: %s" % missing; \
assert all(v == [] for v in hooks.values()), "hooks must be empty"' \
 && case "$(readlink -f /opt/hermes/.venv/bin/python)" in \
      /opt/hermes/tools/python-3.14.7+20260901-linux-x64/bin/python3.14 | \
      /opt/hermes/tools/python-3.14.7+20260901-linux-arm64/bin/python3.14) ;; \
      *) echo "unexpected Python: $(readlink -f /opt/hermes/.venv/bin/python)" >&2; exit 1 ;; \
    esac
# No repository context file (AGENTS.md, .hermes.md, CLAUDE.md, .cursorrules) reaches the
# agent; SOUL.md, Koinon's instructions, does. Hermes has no setting for that, so a two-line
# patch, checked by building a system prompt (no-context-files.py, README.md).
COPY --chmod=0644 no-context-files.py /tmp/no-context-files.py
RUN /opt/hermes/.venv/bin/python -I -B /tmp/no-context-files.py patch \
 && /opt/hermes/.venv/bin/python -I -B /tmp/no-context-files.py check \
 && rm /tmp/no-context-files.py
USER 10001:10001
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-hermes"]
