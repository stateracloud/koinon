// SPDX-License-Identifier: Apache-2.0

// Package forgecredfile names the credential file of an agent's forge token and the binding of its
// envelope (#1086 part C, ADR 0097 decision 12). It holds no key and opens nothing: the reconciler seals
// the token and writes the file, the gateway opens it, and neither may drift from the other.
//
// The file is one entry of the gateway's key Secret:
//
//	forgev2-<org ID>-<agent ID>-<8 hex of the envelope's SHA-256>     ciphertext (envelope v2, opened by the gateway)
//
// The name carries the org ID and the agent ID because the binding of the envelope names them. The name
// is not trusted: a wrong org or agent ID makes the envelope fail to open, and the gateway also checks
// that the agent ID is the agent of the route that names the credential.
package forgecredfile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// The table and the column of the binding. No column stores the token: the reconciler mints it from the
// forge connection and seals it at once. The names only make the binding differ from every stored secret.
const (
	AgentsTable = "agents"
	TokenColumn = "forge_token"
)

// SealedPrefix starts the name of every forge credential file.
const SealedPrefix = "forgev2-"

// Binding is the AAD binding of an agent's forge token.
func Binding(org, agent string) secretstore.Binding {
	return secretstore.Binding{OrgID: org, Table: AgentsTable, RowID: agent, Column: TokenColumn}
}

// SealedName is the file name of a v2 envelope of an agent's forge token.
func SealedName(org, agent pgtype.UUID, envelope []byte) string {
	sum := sha256.Sum256(envelope)
	return SealedPrefix + org.String() + "-" + agent.String() + "-" + hex.EncodeToString(sum[:4])
}

const uuidLen = 36

// ParseSealed reads the org ID and the agent ID from the base name of a forge credential file. It
// reports false for any other name.
func ParseSealed(base string) (org, agent string, ok bool) {
	rest, found := strings.CutPrefix(base, SealedPrefix)
	if !found || len(rest) != 2*uuidLen+1+8+1 || rest[uuidLen] != '-' || rest[2*uuidLen+1] != '-' {
		return "", "", false
	}
	org, agent = rest[:uuidLen], rest[uuidLen+1:2*uuidLen+1]
	for _, c := range rest[2*uuidLen+2:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	var u pgtype.UUID
	if u.Scan(org) != nil || !u.Valid || u.Scan(agent) != nil || !u.Valid {
		return "", "", false
	}
	return org, agent, true
}

// IsSealed reports whether a base name is a forge credential file name.
func IsSealed(base string) bool { return strings.HasPrefix(base, SealedPrefix) }
