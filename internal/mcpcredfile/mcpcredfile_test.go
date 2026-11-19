// SPDX-License-Identifier: Apache-2.0

package mcpcredfile_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/mcpcredfile"
)

func uuid(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

const (
	org    = "11111111-1111-4111-8111-111111111111"
	server = "22222222-2222-4222-8222-222222222222"
)

func TestSealedNameRoundTrips(t *testing.T) {
	name := mcpcredfile.SealedName(uuid(t, org), uuid(t, server), []byte("envelope"))
	if !strings.HasPrefix(name, "mcpv2-"+org+"-"+server+"-") || !mcpcredfile.IsSealed(name) {
		t.Fatalf("name = %q", name)
	}
	o, s, ok := mcpcredfile.ParseSealed(name)
	if !ok || o != org || s != server {
		t.Fatalf("ParseSealed(%q) = %q, %q, %v", name, o, s, ok)
	}
}

func TestPlainNameIsNotSealed(t *testing.T) {
	name := mcpcredfile.PlainName(uuid(t, server), []byte("envelope"))
	if !strings.HasPrefix(name, "mcp-"+server+"-") || mcpcredfile.IsSealed(name) {
		t.Fatalf("name = %q", name)
	}
	if _, _, ok := mcpcredfile.ParseSealed(name); ok {
		t.Fatal("a plaintext name parsed as sealed")
	}
}

func TestNameChangesWithTheEnvelope(t *testing.T) {
	a := mcpcredfile.SealedName(uuid(t, org), uuid(t, server), []byte("a"))
	b := mcpcredfile.SealedName(uuid(t, org), uuid(t, server), []byte("b"))
	if a == b {
		t.Fatal("two envelopes share a file name")
	}
}

func TestParseSealedRefusesBadNames(t *testing.T) {
	good := "mcpv2-" + org + "-" + server + "-0a1b2c3d"
	for _, name := range []string{
		"", "mcpv2-", "mcp-" + server + "-0a1b2c3d", good + "x", good[:len(good)-1],
		"mcpv2-" + org + "_" + server + "-0a1b2c3d",
		"mcpv2-" + strings.Repeat("z", 36) + "-" + server + "-0a1b2c3d",
	} {
		if _, _, ok := mcpcredfile.ParseSealed(name); ok {
			t.Errorf("ParseSealed(%q) accepted a bad name", name)
		}
	}
	if _, _, ok := mcpcredfile.ParseSealed(good); !ok {
		t.Errorf("ParseSealed(%q) refused a good name", good)
	}
}

func TestBinding(t *testing.T) {
	b := mcpcredfile.Binding(org, server)
	if b.OrgID != org || b.RowID != server || b.Table != "mcp_servers" || b.Column != "credential" {
		t.Fatalf("binding = %+v", b)
	}
}
