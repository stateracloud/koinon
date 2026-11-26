# SPDX-License-Identifier: Apache-2.0
#
# The base images of the Go images, as a file that Dependabot reads (#1224). Nothing builds this file.
# Dependabot (docker ecosystem, weekly, 3 day cooldown) proposes new digests here; ko and goreleaser cannot
# be read by it. hack/versions-check.sh fails when .ko.yaml or .goreleaser.yaml names another digest, so a
# base image update is one pull request that changes this file and the two others together.
#
# Re-pin by hand: crane digest cgr.dev/chainguard/static:latest ; crane digest gcr.io/distroless/static:nonroot
# The digest must be at least 3 days old, or carry the security exception (ADR 0079, hack/dep-age-check.sh).

# ko default base (.ko.yaml defaultBaseImage)
FROM cgr.dev/chainguard/static:latest@sha256:599c3cd7597b84aa149c4282beecc8c6d4e1cb5c95dc8d982e70cd0a2a9147a2 AS ko-default
# goreleaser kos base (.goreleaser.yaml base_image)
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS goreleaser-kos
