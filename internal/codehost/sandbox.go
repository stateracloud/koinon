// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"strings"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
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

// The egress prefix of the Koinon gateway (ADR 0097, #1086 part C2). When the gateway serves the
// forge routes, git in the sandbox reaches the code host through <gateway>/egress/<host>/ and sends
// the actor identity token there. koinon-agentd mints the token outside the guest and writes it to
// SandboxEgressToken, which is outside the home: the home is checkpointed, and koinon-agentd never
// writes the token into it (ADR 0120 sections 1 and 5). The write command refuses a token directory
// that is a symbolic link, is not a directory, or is not the writer's own with mode 0700, so an
// accident cannot send the file into the home. These checks do not stop a hostile agent: it can read
// the file and copy the token into the home, and the token's short lifetime limits that. A memory
// snapshot of the whole guest would hold the token; no production path takes one today. The token
// lives 300 to 3600 seconds, and agentd writes a new one before the old one ends.
//
// INTERIM (ADR 0120, amendment of 2026-11-26): the agent user can read SandboxEgressToken while the
// run is live, because git reads it in the guest. The gateway accepts the token on every egress route
// in scope for its org and agent. koinon-agentd writes the token to no other place; after the write
// command, the escape suite fails if the token is in any other place that it scans. Exit: issue #1386,
// where a hop outside the guest adds the header and no token is in the guest. Then remove the file and
// this text.
const (
	// SandboxEgressDir holds the token file. It is not under /sandbox.
	SandboxEgressDir = "/tmp/.koinon-egress"
	// SandboxEgressToken is the git configuration that carries the actor token. SandboxGitConfig
	// includes it; git ignores an include that does not exist.
	SandboxEgressToken = SandboxEgressDir + "/token.gitconfig"
	// ActorTokenHeader is the header that carries the actor token (route.TokenHeader).
	//nolint:gosec // a header name, not a credential
	ActorTokenHeader = "Koinon-Actor-Token"
)

// GitScriptMarker is in the first line of every script that writes git configuration into a
// sandbox, for logs and for tests that fake the sandbox.
const GitScriptMarker = "koinon-gitconfig"

// writeEgressTokenScript replaces $2 (a file directly in $1) with stdin in one rename, in the
// directory $1, which is outside the home. The guest can write to /tmp, so the script refuses a $1
// that is a symbolic link, is not a directory, or is not this user's own with mode 0700: a link or a
// directory planted before the run could otherwise send each token into the checkpointed home. After
// the checks it works in the directory itself (cd -P, then relative names), so a later rename of the
// path does not move the write, and mv -T never moves the file into a directory planted at $2.
const writeEgressTokenScript = `# ` + GitScriptMarker + `
umask 077
if [ -L "$1" ] || { [ -e "$1" ] && [ ! -d "$1" ]; }; then echo "$1 is not a plain directory" >&2; exit 1; fi
mkdir -p -- "$1" && cd -P -- "$1" || exit 1
if [ "$(pwd -P)" != "$1" ] || [ "$(stat -c %u:%a .)" != "$(id -u):700" ]; then echo "$1 is not a plain directory of this user with mode 0700" >&2; exit 1; fi
f=${2##*/}
t=$(mktemp .git.XXXXXX) && cat > "$t" && rm -rf -- "$f" && mv -fT -- "$t" "$f"`

// removeEgressTokenScript removes the token file.
const removeEgressTokenScript = `# ` + GitScriptMarker + `
rm -f -- "$2"`

// WriteEgressTokenCommand is the sandbox command that writes SandboxEgressToken from its stdin (the
// output of EgressTokenGitConfig). koinon-agentd runs it through the driver's exec, and the escape
// suite runs the same command, so the suite checks the place that production writes.
func WriteEgressTokenCommand() []string {
	return []string{"/bin/sh", "-c", writeEgressTokenScript, "sh", SandboxEgressDir, SandboxEgressToken}
}

// RemoveEgressTokenCommand is the sandbox command that removes SandboxEgressToken.
func RemoveEgressTokenCommand() []string {
	return []string{"/bin/sh", "-c", removeEgressTokenScript, "sh", SandboxEgressDir, SandboxEgressToken}
}

// EgressPrefix returns the gateway URL that git uses for the code host at loc: base is the gateway's
// egress listener (http://host:port, no trailing slash).
func EgressPrefix(base string, loc connectors.Location) string {
	return strings.TrimRight(base, "/") + "/egress/" + loc.Web.Authority() + "/"
}

// EgressGitConfig renders what SandboxGitConfig adds when git goes through the gateway: the include
// of the token file, the URL rewrite of the code host to the gateway prefix, and an empty credential
// helper for the gateway, so no helper is asked for a credential that only the gateway holds. It holds
// nothing secret.
func EgressGitConfig(base string, loc connectors.Location) []byte {
	var b strings.Builder
	b.WriteString("# Git reaches the code host through the Koinon gateway, which attaches the credential.\n")
	b.WriteString("[include]\n")
	b.WriteString("\tpath = " + SandboxEgressToken + "\n")
	writeEgressRewrite(&b, base, loc)
	return []byte(b.String())
}

// EgressPlaceholderGitConfig renders what SandboxGitConfig adds when git goes through the gateway and
// the actor identity is added outside the guest (Agent Substrate, issue #1386, ADR 0120 amendment of
// 2026-11-26). It is EgressGitConfig without the include of a token file: git sends the public
// actortoken.Placeholder in the actor token header to the gateway's egress prefix, and atenet-egress
// replaces it with the actor's token on its way out of the guest. No token is in the guest. It holds
// nothing secret.
func EgressPlaceholderGitConfig(base string, loc connectors.Location) []byte {
	var b strings.Builder
	b.WriteString("# Git reaches the code host through the Koinon gateway, which attaches the credential.\n")
	b.WriteString("# The egress layer outside the guest replaces the placeholder with the actor's token.\n")
	writeEgressRewrite(&b, base, loc)
	b.WriteString("[http \"" + strings.TrimRight(base, "/") + "/egress/\"]\n")
	b.WriteString("\textraHeader = " + ActorTokenHeader + ": " + actortoken.Placeholder + "\n")
	return []byte(b.String())
}

// writeEgressRewrite writes the URL rewrite of the code host at loc to the gateway prefix and the
// empty credential helper for the gateway, so no helper is asked for a credential that only the
// gateway holds.
func writeEgressRewrite(b *strings.Builder, base string, loc connectors.Location) {
	b.WriteString("[url \"" + EgressPrefix(base, loc) + "\"]\n")
	b.WriteString("\tinsteadOf = https://" + loc.Web.Authority() + "/\n")
	b.WriteString("[credential \"" + strings.TrimRight(base, "/") + "/egress/\"]\n")
	b.WriteString("\thelper =\n")
}

// EgressTokenGitConfig renders SandboxEgressToken: the actor token as an extra header that git sends
// only to the gateway's egress prefix. ok is false for a token that is not a compact JWS (three
// base64url parts), so a value can never add a line to the file.
func EgressTokenGitConfig(base, token string) (cfg []byte, ok bool) {
	if !compactJWS(token) {
		return nil, false
	}
	var b strings.Builder
	b.WriteString("# Rendered by koinon-agentd; the actor token is short-lived and replaced before it ends.\n")
	b.WriteString("[http \"" + strings.TrimRight(base, "/") + "/egress/\"]\n")
	b.WriteString("\textraHeader = " + ActorTokenHeader + ": " + token + "\n")
	return []byte(b.String()), true
}

// notBase64URL reports a rune outside the unpadded base64url alphabet.
func notBase64URL(c rune) bool {
	return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_'
}

// compactJWS reports whether s is three non-empty base64url parts, so it cannot add a line to a git config.
func compactJWS(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if strings.IndexFunc(p, notBase64URL) >= 0 {
			return false
		}
	}
	return true
}
