// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// ProbeWakeChannel is the Postgres NOTIFY channel on which the gateway asks the prober to sync one
// server now (#1173). The gateway sends it when a server lists a tool whose text is neither the
// approved text nor the text the sync recorded last: the sync has not seen the change yet. The
// reconciler that leads LISTENs on it and probes that server at once, rate-limited per server, instead
// of at the next probe interval. The payload is ProbeWakePayload; the channel carries no tool text.
const ProbeWakeChannel = "koinon_mcp_probe"

// ProbeWakePayload is the payload of a ProbeWakeChannel notification: "<org id>/<server id>".
func ProbeWakePayload(org, server pgtype.UUID) string {
	return org.String() + "/" + server.String()
}

// ParseProbeWakePayload reads a ProbeWakePayload. ok is false for anything else.
func ParseProbeWakePayload(p string) (org, server pgtype.UUID, ok bool) {
	o, s, found := strings.Cut(p, "/")
	if !found {
		return org, server, false
	}
	if org.Scan(o) != nil || server.Scan(s) != nil || !org.Valid || !server.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	return org, server, true
}
