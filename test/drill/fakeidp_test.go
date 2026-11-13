// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/stateracloud/koinon/test/drill"
)

func TestFakeIdPManifestIsLabelledAndHoldsTheDexConfig(t *testing.T) {
	const ns = "koinon-drill-fakes-a1b2c3"
	m, err := drill.FakeIdPManifest(ns, "a1b2c3", "ghcr.io/dexidp/dex:v2.46.1@sha256:abc", drill.IdPUsers())
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 3 {
		t.Fatalf("want a Secret, a Deployment and a Service, got %d documents", len(docs))
	}
	var secret struct {
		Metadata   struct{ Labels map[string]string }
		StringData map[string]string `yaml:"stringData"`
	}
	for i, d := range docs {
		var v struct {
			Metadata struct{ Labels map[string]string }
		}
		if err := yaml.Unmarshal([]byte(d), &v); err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
		if v.Metadata.Labels[drill.RunLabel] != "a1b2c3" {
			t.Errorf("document %d has no run label (the sweep would miss it)", i)
		}
	}
	if err := yaml.Unmarshal([]byte(docs[0]), &secret); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Issuer  string
		Storage struct{ Type string }
		OAuth2  struct {
			SkipApprovalScreen bool `yaml:"skipApprovalScreen"`
			PKCE               struct{ Enforce bool }
		}
		EnablePasswordDB bool `yaml:"enablePasswordDB"`
		StaticPasswords  []struct {
			Email, Hash, Username string
			UserID                string `yaml:"userID"`
			Groups                []string
		} `yaml:"staticPasswords"`
		StaticClients []struct {
			ID           string `yaml:"id"`
			Public       bool
			RedirectURIs []string `yaml:"redirectURIs"`
			Secret       string
		} `yaml:"staticClients"`
	}
	if err := yaml.Unmarshal([]byte(secret.StringData["config.yaml"]), &cfg); err != nil {
		t.Fatalf("the Dex config does not parse: %v", err)
	}
	if cfg.Issuer != "http://dex."+ns+".svc.cluster.local:5556/dex" || cfg.Storage.Type != "memory" || !cfg.OAuth2.SkipApprovalScreen || !cfg.OAuth2.PKCE.Enforce || !cfg.EnablePasswordDB {
		t.Errorf("config: %+v", cfg)
	}
	if len(cfg.StaticClients) != 1 || cfg.StaticClients[0].ID != "koinon-drill" || !cfg.StaticClients[0].Public || cfg.StaticClients[0].Secret != "" {
		t.Errorf("want one public client without a secret: %+v", cfg.StaticClients)
	}
	if got := cfg.StaticClients[0].RedirectURIs; len(got) != 2 || got[0] != drill.OIDCRedirect(drill.OIDCSourcePort) || got[1] != drill.OIDCRedirect(drill.OIDCTargetPort) {
		t.Errorf("redirect URIs: %v", got)
	}
	want := map[string]string{"drill-admin": "koinon-admins", "drill-member": "koinon-users", "drill-disabled": "koinon-users"}
	if len(cfg.StaticPasswords) != len(want) {
		t.Fatalf("users: %+v", cfg.StaticPasswords)
	}
	ids := map[string]bool{}
	for _, u := range cfg.StaticPasswords {
		if len(u.Groups) != 1 || u.Groups[0] != want[u.Username] {
			t.Errorf("%s: groups %v", u.Username, u.Groups)
		}
		if bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(drill.FakeIdPPassword)) != nil {
			t.Errorf("%s: the hash is not a bcrypt hash of the drill password", u.Username)
		}
		ids[u.UserID] = true
	}
	if len(ids) != len(want) {
		t.Errorf("user IDs must differ: %v", ids)
	}
}

// No bcrypt hash and no password-like secret may be in the repository: the hash is made at run time.
func TestNoBcryptHashIsCommittedInTheDrill(t *testing.T) {
	err := filepath.Walk(".", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// The markers are built here, so that this file does not hold one.
		if strings.Contains(string(b), "$2"+"a$") || strings.Contains(string(b), "$2"+"b$") {
			t.Errorf("%s holds a bcrypt hash", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTheIdPIssuerIsOneStringForEveryClient(t *testing.T) {
	if got := drill.FakeIdPIssuer("koinon-drill-fakes-a1b2c3"); got != "http://dex.koinon-drill-fakes-a1b2c3.svc.cluster.local:5556/dex" {
		t.Errorf("issuer %q", got)
	}
	if got := drill.FakeIdPHost("x"); got != "dex.x.svc.cluster.local" {
		t.Errorf("host %q", got)
	}
}
