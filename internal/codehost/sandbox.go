// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"strings"

	"github.com/stateracloud/koinon/connectors"
)

// What a connection's host adds to the git setup of a sandbox (ADR 0062). The harness images
// carry git's configuration for the default host (harnesses/common/etc/koinon/git/config) and
// include SandboxGitConfig, which koinon-agentd writes before every harness start.
const (
	// SandboxGitConfig is the git configuration rendered from the connection's host.
	SandboxGitConfig = "/sandbox/.koinon/git/config"
	// SandboxGitCA is the CA file git trusts for the connection's host: the sandbox's system
	// roots plus the connection's CA bundle.
	SandboxGitCA = "/sandbox/.koinon/git/ca.pem"
	// CredentialHelper is git's credential helper in the harness images. It takes --host.
	//nolint:gosec // a path, not a credential
	CredentialHelper = "/usr/local/bin/koinon-git-credential"
)

// GitConfig renders the sandbox's git configuration for a connection at loc: the credential
// helper scoped to the connection's host and, with a CA bundle, http.sslCAInfo for it. For the
// default host the image's own configuration already holds the credential section, so only the
// CA is added; for the default host without a CA the file only says so. It holds nothing secret
// (the helper prints a placeholder, a CA bundle is public).
func GitConfig(loc connectors.Location, hasCA bool) []byte {
	var b strings.Builder
	b.WriteString("# SPDX-License-Identifier: Apache-2.0\n#\n")
	b.WriteString("# Rendered by koinon-agentd from the connection's host before every run; changes are lost.\n")
	b.WriteString("# Nothing secret: credentials come from koinon-git-credential, and a CA bundle is public.\n")
	url := "https://" + loc.Web.Authority()
	if !loc.Default {
		b.WriteString("[credential \"" + url + "\"]\n")
		b.WriteString("\t# The empty value drops any helper configured before this one.\n")
		b.WriteString("\thelper =\n")
		b.WriteString("\thelper = " + CredentialHelper + " --host " + loc.Web.Authority() + "\n")
		b.WriteString("\tuseHttpPath = false\n")
	}
	if hasCA {
		b.WriteString("[http \"" + url + "/\"]\n")
		b.WriteString("\tsslCAInfo = " + SandboxGitCA + "\n")
	}
	return []byte(b.String())
}
