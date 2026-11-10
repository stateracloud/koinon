// SPDX-License-Identifier: Apache-2.0

package project

const (
	// DocsURL is the address of the documentation site (the repository stateracloud/koinon-docs,
	// ADR 0108). Messages that point an operator to a page build the address from it.
	DocsURL = "https://koinon.stateracloud.com"

	// SecretKeysDocURL is the operator page about the key-encryption keys and the other install secrets.
	SecretKeysDocURL = DocsURL + "/docs/operate/secret-keys/"

	// InstallAuditDocURL is the section of that page about the install audit chain and its database role.
	InstallAuditDocURL = SecretKeysDocURL + "#the-install-audit-chain-adr-0100"
)
