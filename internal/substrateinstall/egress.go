// SPDX-License-Identifier: Apache-2.0

package substrateinstall

// Corrections of the atenet-egress gateway (Envoy) in the pinned Substrate tree. They apply on every platform,
// because the egress manifest (manifests/ate-install/atenet-egress.yaml) is the same on every route.
//
// 1. No upstream TLS session reuse (issue #1486). Envoy keeps ONE upstream TLS session cache for each forward-proxy
// cluster. The cache is not keyed by SNI and keeps only the newest session (max_session_keys defaults to 1). With
// TLS 1.2 as the client maximum, Cloudflare resumes a session of one of its hosts under the SNI of another host.
// Envoy checks the stored certificate again (auto_san_validation), sees the wrong name and refuses the call. A
// failed resume stores no new session, so every later call to that host fails for all actors until the pod restarts.
// max_session_keys: 0 turns the cache off. The cache is also shared by all actors, so it must not carry a session
// from one actor to another.
//
// 2. IPv4-first DNS (option). Substrate resolves with dns_lookup_family ALL. On a cluster without an IPv6 route,
// a call can fail at random with 503 "Network is unreachable". V4_PREFERRED tries IPv4 first. It is off by default.
//
// Exit condition for 1: remove it when a Substrate release sets max_session_keys: 0 or keys the sessions by SNI
// (https://github.com/stateracloud/koinon/issues/1486). Exit condition for 2: none, it is an option.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEgress is returned when the pinned tree does not hold what an egress correction expects.
var ErrEgress = errors.New("egress gateway")

const (
	egressManifest = "manifests/ate-install/atenet-egress.yaml"

	// egressTLSContextOld is the head of the UpstreamTlsContext of the two TLS-originating forward-proxy
	// clusters, egress_forward_proxy and egress_forward_proxy_grpc. The other clusters send no TLS.
	egressTLSContextOld = `            "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
            common_tls_context:
`
	// egressTLSContextNew carries max_session_keys: 0 (a field of UpstreamTlsContext, not of common_tls_context).
	egressTLSContextNew = `            "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
            # Koinon, issue #1486: no TLS session cache. See internal/substrateinstall/egress.go.
            max_session_keys: 0
            common_tls_context:
`
	egressDNSOld = "dns_lookup_family: ALL\n"
	egressDNSNew = "dns_lookup_family: V4_PREFERRED\n"
)

// ApplyEgressCorrections edits the egress manifest of the exported tree at dir, before ate-setup reads it. Each
// edit names the text it expects and fails when the pinned tree does not hold it exactly as many times as expected.
func (p Profile) ApplyEgressCorrections(dir string) error {
	path := filepath.Join(dir, filepath.FromSlash(egressManifest))
	if err := replaceN(path, egressTLSContextOld, egressTLSContextNew, 2); err != nil {
		return err
	}
	if p.EgressDNSV4Preferred {
		// One anchor (&egress_dns_cache) holds the setting; the clusters and the listeners alias it.
		return replaceN(path, egressDNSOld, egressDNSNew, 1)
	}
	return nil
}

// replaceN replaces the n occurrences of old in the file, and fails when there are not exactly n.
func replaceN(path, old, replacement string, n int) error {
	raw, err := os.ReadFile(path) //nolint:gosec // inside the exported tree the installer made
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEgress, err)
	}
	if got := strings.Count(string(raw), old); got != n {
		return fmt.Errorf("%w: %s holds %d copies of %q, want %d: the pinned tree changed and Koinon's corrections no longer fit", ErrEgress, filepath.Base(path), got, strings.TrimSpace(old), n)
	}
	return os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), old, replacement)), 0o600) //nolint:gosec // inside the exported tree the installer made
}
