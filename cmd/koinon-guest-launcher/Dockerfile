# syntax=docker/dockerfile:1.19@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# SPDX-License-Identifier: Apache-2.0
#
# Substrate overlay of a Koinon harness image (ADR 0119, ADR 0015 and 0016 as amended, issue #1087):
# the harness image with the guest front (koinon-guest-front, #1309) and koinon-guest-launcher with its two
# policies added. The actor's process is the guest front: Substrate starts the image ENTRYPOINT. The front embeds
# the upstream guest server (github.com/agent-substrate/env/guest, unchanged) and passes a call to it only with a
# token that the driver signed, so agent code cannot use the guest API. The ProcessService runs the harness. The launcher contract is an exec prefix
# (harness.Descriptor.LauncherPrefix): the substrate driver runs the harness over the ProcessService, so the
# harness is a child of the guest front and never of the ENTRYPOINT. Only the substrate driver uses this
# image. The OpenShell driver (the default) keeps the plain harness image and its exec path.
#
# Build: make harness-substrate-opencode. Both binaries come from the named build context "launcher"
# (build/guest-launcher/linux-<arch>/, `make guest-launcher`); the harness image is the named build context
# "flavor". A local build passes it as an OCI layout (hack/harness-flavor-context.sh), so a docker-container builder
# needs no registry; the release passes docker-image://<digest reference> (hack/release-harness-substrate.sh). There
# is no RUN step, so a multi-platform build needs no emulation.
# "flavor" is a named build context, not a stage
# hadolint ignore=DL3006
# dockerfile-lint: allow=digest flavor is a named build context; the caller passes the harness image pinned by digest (OCI layout or docker-image://<digest>)
FROM flavor
ARG TARGETARCH
# "launcher" is a named build context, not a stage
# hadolint ignore=DL3022
COPY --from=launcher --chmod=0755 linux-${TARGETARCH}/koinon-guest-launcher /usr/local/bin/koinon-guest-launcher
# hadolint ignore=DL3022
COPY --from=launcher --chmod=0755 linux-${TARGETARCH}/koinon-guest-front /usr/local/bin/koinon-guest-front
# Two policies, one per tier family (ADR 0120 section 4.2 and 4.3): the vm tier fails closed without
# Landlock, the gVisor tiers (hardened, dev) run it best effort (their root is writable by decision, issue #1150).
COPY --chmod=u=rwX,go=rX policy-vm.json policy-gvisor.json /etc/koinon/guest-launcher/
# The guest front listens on port 80 (its default, which the Substrate router expects) and runs the payload as
# its own user, so it runs as root: a non-root user has neither the port nor the right to start a process as
# another user. The payload is hardened by the launcher (no_new_privs, capability drop, seccomp, Landlock), and
# the user mode stays best_effort until Substrate has a payload uid/gid option (ADR 0120 section 4, decision 2026-11-18).
USER root
WORKDIR /sandbox
ENTRYPOINT ["/usr/local/bin/koinon-guest-front", "-workspace", "/sandbox"]
