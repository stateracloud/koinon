// SPDX-License-Identifier: Apache-2.0

// Package mcpcredfile names the credential files of MCP servers and the binding of a stored MCP
// credential (ADR 0097 decision 12). It holds no key and opens nothing: the reconciler, the gateway and
// the writer of the credential share these names, and none of them may drift from the others.
//
// A credential reaches the gateway as one file in the key Secret. Two kinds exist, told apart by the file name:
//
//	mcp-<server ID>-<8 hex of the envelope's SHA-256>                  plaintext (envelope v1, opened by the reconciler)
//	mcpv2-<org ID>-<server ID>-<8 hex of the envelope's SHA-256>       ciphertext (envelope v2, opened by the gateway)
//
// A v2 name carries the org ID because the binding of the envelope names it (the gateway has the org slug
// only). The name is not trusted: a wrong org or server ID makes the envelope fail to open.
package mcpcredfile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// The table and the column of a stored MCP server credential, as the binding of its envelope names them.
const (
	ServersTable     = "mcp_servers"
	CredentialColumn = "credential"
)

// File name prefixes.
const (
	PlainPrefix  = "mcp-"
	SealedPrefix = "mcpv2-"
)

// Binding is the AAD binding of a server's credential.
func Binding(org, server string) secretstore.Binding {
	return secretstore.Binding{OrgID: org, Table: ServersTable, RowID: server, Column: CredentialColumn}
}

func digest(envelope []byte) string {
	sum := sha256.Sum256(envelope)
	return hex.EncodeToString(sum[:4])
}

// PlainName is the file name of the plaintext credential of a v1 envelope.
func PlainName(server pgtype.UUID, envelope []byte) string {
	return PlainPrefix + server.String() + "-" + digest(envelope)
}

// SealedName is the file name of a v2 envelope, copied as it is.
func SealedName(org, server pgtype.UUID, envelope []byte) string {
	return SealedPrefix + org.String() + "-" + server.String() + "-" + digest(envelope)
}

// uuidLen is the length of a UUID in its text form.
const uuidLen = 36

// ParseSealed reads the org ID and the server ID from the base name of a v2 credential file. It reports
// false for any other name, including a plaintext credential file.
func ParseSealed(base string) (org, server string, ok bool) {
	rest, found := strings.CutPrefix(base, SealedPrefix)
	if !found || len(rest) != 2*uuidLen+1+8+1 || rest[uuidLen] != '-' || rest[2*uuidLen+1] != '-' {
		return "", "", false
	}
	org, server = rest[:uuidLen], rest[uuidLen+1:2*uuidLen+1]
	for _, c := range rest[2*uuidLen+2:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	var u pgtype.UUID
	if u.Scan(org) != nil || !u.Valid || u.Scan(server) != nil || !u.Valid {
		return "", "", false
	}
	return org, server, true
}

// IsSealed reports whether a base name is a v2 credential file name.
func IsSealed(base string) bool { return strings.HasPrefix(base, SealedPrefix) }
