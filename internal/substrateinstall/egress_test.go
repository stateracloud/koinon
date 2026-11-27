// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/substrateinstall"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

const egressPath = "manifests/ate-install/atenet-egress.yaml"

// egressFixture has the shape of the pinned atenet-egress.yaml: one DNS cache anchor, two clusters that originate
// TLS and one cleartext cluster.
const egressFixture = `apiVersion: v1
kind: ConfigMap
metadata:
  name: atenet-egress
  namespace: ate-system
data:
  envoy.yaml: |
    static_resources:
      clusters:
      - name: egress_forward_proxy
        cluster_type:
          typed_config:
            dns_cache_config: &egress_dns_cache
              name: egress_dns_cache
              dns_lookup_family: ALL
        transport_socket:
          name: envoy.transport_sockets.tls
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
            common_tls_context:
              validation_context:
                trusted_ca:
                  filename: /etc/ssl/certs/ca-certificates.crt

      - name: egress_forward_proxy_grpc
        cluster_type:
          typed_config:
            dns_cache_config: *egress_dns_cache
        transport_socket:
          name: envoy.transport_sockets.tls
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
            common_tls_context:
              validation_context:
                trusted_ca:
                  filename: /etc/ssl/certs/ca-certificates.crt

      - name: egress_forward_proxy_cleartext
        cluster_type:
          typed_config:
            dns_cache_config: *egress_dns_cache
`

// envoyClusters parses the envoy.yaml of the egress ConfigMap into its clusters by name.
func envoyClusters(g Gomega, manifest string) map[string]map[string]any {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	// The pinned file has several YAML documents; the ConfigMap is the one that holds envoy.yaml.
	for _, doc := range strings.Split(manifest, "\n---\n") {
		var d struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
		}
		if yaml.Unmarshal([]byte(doc), &d) == nil && d.Kind == "ConfigMap" && d.Data["envoy.yaml"] != "" {
			cm.Data = d.Data
		}
	}
	var cfg struct {
		StaticResources struct {
			Clusters []map[string]any `json:"clusters"`
		} `json:"static_resources"`
	}
	g.Expect(yaml.Unmarshal([]byte(cm.Data["envoy.yaml"]), &cfg)).To(Succeed())
	out := map[string]map[string]any{}
	for _, c := range cfg.StaticResources.Clusters {
		out[c["name"].(string)] = c
	}
	return out
}

func tlsContext(c map[string]any) map[string]any {
	ts, _ := c["transport_socket"].(map[string]any)
	tc, _ := ts["typed_config"].(map[string]any)
	return tc
}

func dnsFamily(c map[string]any) any {
	ct := c["cluster_type"].(map[string]any)["typed_config"].(map[string]any)
	return ct["dns_cache_config"].(map[string]any)["dns_lookup_family"]
}

// checkEgressTLSCache is the check of issue #1486: both TLS-originating forward-proxy clusters turn the upstream
// TLS session cache off, and no other cluster gets the setting.
func checkEgressTLSCache(g Gomega, clusters map[string]map[string]any) {
	for _, name := range []string{"egress_forward_proxy", "egress_forward_proxy_grpc"} {
		c, ok := clusters[name]
		g.Expect(ok).To(BeTrue(), name)
		tc := tlsContext(c)
		g.Expect(tc).To(HaveKeyWithValue("max_session_keys", BeEquivalentTo(0)), "%s must set max_session_keys: 0 (issue #1486)", name)
		g.Expect(tc).To(HaveKey("common_tls_context"), name)
	}
	for name, c := range clusters {
		if name == "egress_forward_proxy" || name == "egress_forward_proxy_grpc" {
			continue
		}
		g.Expect(tlsContext(c)).NotTo(HaveKey("max_session_keys"), name)
	}
}

func TestEgressCorrectionSetsMaxSessionKeysOnBothTLSClusters(t *testing.T) {
	g := NewWithT(t)
	dir := writeTree(t, map[string]string{egressPath: egressFixture})
	g.Expect(substrateinstall.Profile{}.ApplyEgressCorrections(dir)).To(Succeed())
	out := readFile(t, dir, egressPath)
	checkEgressTLSCache(g, envoyClusters(g, out))
	g.Expect(dnsFamily(envoyClusters(g, out)["egress_forward_proxy"])).To(Equal("ALL"), "IPv4-first is off by default")
}

func TestEgressCorrectionIPv4FirstIsAnOption(t *testing.T) {
	g := NewWithT(t)
	dir := writeTree(t, map[string]string{egressPath: egressFixture})
	g.Expect(substrateinstall.Profile{EgressDNSV4Preferred: true}.ApplyEgressCorrections(dir)).To(Succeed())
	clusters := envoyClusters(g, readFile(t, dir, egressPath))
	checkEgressTLSCache(g, clusters)
	for name, c := range clusters {
		g.Expect(dnsFamily(c)).To(Equal("V4_PREFERRED"), name)
	}
}

func TestEgressCorrectionRefusesATreeThatChanged(t *testing.T) {
	g := NewWithT(t)
	// One TLS context only: a pin that dropped a cluster no longer fits.
	dir := writeTree(t, map[string]string{egressPath: "data:\n  envoy.yaml: |\n    x:\n            \"@type\": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext\n            common_tls_context:\n"})
	g.Expect(substrateinstall.Profile{}.ApplyEgressCorrections(dir)).To(MatchError(ContainSubstring("holds 1 copies")))
	// A tree without the DNS family text, with the option on.
	dir = writeTree(t, map[string]string{egressPath: egressFixture})
	raw := strings.Replace(readFile(t, dir, egressPath), "dns_lookup_family: ALL", "dns_lookup_family: V4_ONLY", 1)
	g.Expect(os.WriteFile(filepath.Join(dir, egressPath), []byte(raw), 0o600)).To(Succeed())
	g.Expect(substrateinstall.Profile{EgressDNSV4Preferred: true}.ApplyEgressCorrections(dir)).To(MatchError(ContainSubstring("dns_lookup_family: ALL")))
}

// TestEgressCorrectionFitsThePinnedTree renders the real pinned manifest with the corrections.
func TestEgressCorrectionFitsThePinnedTree(t *testing.T) {
	testenv.RequireT(t, substrateSource, gitBinary)
	g := NewWithT(t)
	inst := &substrateinstall.Installer{Runner: execRunner{}, SourceRepo: os.Getenv("KOINON_SUBSTRATE_SOURCE"), Out: io.Discard}
	dir := filepath.Join(t.TempDir(), "src")
	g.Expect(inst.ExportPin(t.Context(), t.TempDir(), dir)).To(Succeed())
	g.Expect(substrateinstall.Profile{EgressDNSV4Preferred: true}.ApplyEgressCorrections(dir)).To(Succeed())
	clusters := envoyClusters(g, readFile(t, dir, egressPath))
	checkEgressTLSCache(g, clusters)
	g.Expect(dnsFamily(clusters["egress_forward_proxy"])).To(Equal("V4_PREFERRED"))
}
