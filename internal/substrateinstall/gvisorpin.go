// SPDX-License-Identifier: Apache-2.0

package substrateinstall

// The gVisor release this Koinon release stages in the cluster object store (issue #1157).
//
// It is the release of the SandboxConfig `gvisor-default` that the pinned Substrate ships
// (manifests/ate-install/sandboxconfig-gvisor.yaml, version v7 of commit PinnedCommit). That config points at
// gs://gvisor, and atelet then reads the tarball from the internet. Koinon keeps the namespace of Substrate
// closed to the internet (ADR 0120 section 6), so the installer puts the same tarball in the cluster store.
//
// The values are the same as GVISOR_VERSION and GVISOR_SHA256_AMD64/ARM64 in hack/versions.env. A test
// compares the two. To bump: new values in both places, and a new SandboxConfig version name follows from
// the release (the old version stays, see gvisorsandboxconfig.go).
const (
	GVisorRelease     = "20261102.0"
	GVisorSHA256AMD64 = "a1439ffbba7d212e0fde32ea79fa9bb51cbc4c5f6c16af1f047260290b861d42"
	GVisorSHA256ARM64 = "f702ae92ad7a0c4134be34ca23f52966dbb696db0c0f5b8ec439210ab2cd7c6a"

	// GVisorDefaultMirror is the public bucket of the gVisor project. An operator mirror has the same layout
	// below its base URL: <base>/<release>/<x86_64|aarch64>/gvisor.tar.zstd.
	GVisorDefaultMirror = "https://storage.googleapis.com/gvisor/releases/release"
)

// gvisorArch is one architecture of the gVisor release.
type gvisorArch struct {
	// goArch is the key of SandboxConfig assets (GOARCH).
	goArch string
	// dir is the directory name of the gVisor release bucket.
	dir    string
	sha256 string
}

var gvisorArches = []gvisorArch{
	{"amd64", "x86_64", GVisorSHA256AMD64},
	{"arm64", "aarch64", GVisorSHA256ARM64},
}
