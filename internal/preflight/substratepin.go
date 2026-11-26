// SPDX-License-Identifier: Apache-2.0

package preflight

// The Substrate release this Koinon release installs and tests (ADR 0119 D4). This is the one place the
// pin lives: the preflight accepts exactly this version, and the installer step (internal/substrateinstall)
// refuses an ate-setup or a source checkout that is not this commit.
//
// Substrate releases ship no ate-setup binary (only kubectl-ate, with .sha256 files), and ate-setup needs
// the manifests of a source checkout, so the pin is the commit of the release tag, not a binary digest:
// `git rev-parse v1.0.1^{commit}` in github.com/agent-substrate/substrate.
const (
	SubstratePinnedVersion = "v1.0.1"
	SubstratePinnedCommit  = "20778a6dbb01f13024642851bb592bf67c9fec05"
	// SubstratePinnedImageRepo is the registry path of the release images (ate-setup --image-repo).
	// The images are pinned by tag (the release version), not by digest: image digests get pinned here when
	// Substrate publishes them with the release. They are named by component under this path, for example
	// ghcr.io/agent-substrate/substrate/ateapi:v1.0.1 (seen on koinon-lab, #1089).
	SubstratePinnedImageRepo = "ghcr.io/agent-substrate/substrate"
)
