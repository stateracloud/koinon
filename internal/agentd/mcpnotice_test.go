// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

func TestMCPNoticeText(t *testing.T) {
	if got := mcpNoticeText(nil); got != "" {
		t.Fatalf("no unavailable servers must give no notice, got %q", got)
	}
	got := mcpNoticeText([]store.ListUnavailableMCPServersOfAgentRow{
		{Name: "tracker", Health: "unhealthy", HealthReason: pgtype.Text{String: "connection refused", Valid: true}},
		{Name: "wiki", Health: "unknown", DisabledReason: pgtype.Text{String: "its Kubernetes Secret was deleted", Valid: true}},
		{Name: "docs", Health: "unhealthy"},
		{Name: "vault", Health: "healthy", CredentialLost: true},
	})
	for _, want := range []string{"- tracker: connection refused", "- wiki: its Kubernetes Secret was deleted", "- docs: it is not responding", "- vault: its credential was lost and an admin has to enter it again", "not available right now"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q:\n%s", want, got)
		}
	}
}
