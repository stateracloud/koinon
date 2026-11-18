// SPDX-License-Identifier: Apache-2.0

package preflight

// The Substrate release this Koinon release installs and tests (ADR 0119 D4). This is the one place the
// pin lives: the preflight accepts exactly this version, and the installer step (internal/substrateinstall)
// refuses an ate-setup or a source checkout that is not this commit.
//
// Substrate releases ship no ate-setup binary (only kubectl-ate, with .sha256 files), and ate-setup needs
// the manifests of a source checkout, so the pin is the commit of the release tag, not a binary digest:
// `git rev-parse v1.0.0^{commit}` in github.com/agent-substrate/substrate.
const (
	SubstratePinnedVersion = "v1.0.0"
	SubstratePinnedCommit  = "b34fbee742906494b5eb4f1f36987176bf8770a4"
	// SubstratePinnedImageRepo is the registry path of the release images (ate-setup --image-repo).
	// Unverified: taken from docs/dev/releasing.md of the release, not pulled.
	SubstratePinnedImageRepo = "ghcr.io/agent-substrate/substrate"
)
