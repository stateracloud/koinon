// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"fmt"
	"regexp"
	"strings"
)

// pinnedDigest is the end of a reference pinned by digest. It is the same rule as the digest rule of
// the Substrate admission policy (charts/koinon/templates/substrate-policy.yaml).
var pinnedDigest = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

// RequireDigest refuses an actor image reference that is not pinned by digest (issue #1192). A
// reference with a tag only can serve other bytes when a registry or a cache moves the tag, and
// nothing would see it. With a digest, the node agent checks the manifest and every layer against
// it, so neither the registry nor a pull-through cache needs trust (ADR 0120 section 6). The tag is
// optional: "name@sha256:<hex>" and "name:tag@sha256:<hex>" both pass, and the digest wins.
//
// The driver refuses and does not resolve a tag to a digest: a resolve trusts the registry at that
// moment, and it needs registry access and pull credentials in the control plane. The digest is set
// where the image is defined (the chart's harnesses.<name>.image.digest), as for every other image
// in the chart.
func RequireDigest(ref string) error {
	if ref == "" {
		return fmt.Errorf("actor image is empty: it must be pinned by digest (name@sha256:<64 hex digits>)")
	}
	loc := pinnedDigest.FindStringIndex(ref)
	if loc == nil || loc[0] == 0 || strings.Count(ref, "@") != 1 {
		return fmt.Errorf("actor image %q is not pinned by digest: use name@sha256:<64 hex digits> or name:tag@sha256:<64 hex digits>", ref)
	}
	return nil
}

// RewriteImage rewrites the registry of an actor image reference to the image source of the node
// agent (ADR 0061, ADR 0120 section 6, issue #1170). It is the Go twin of the chart helper
// koinon.imageRef and follows its rules:
//
//   - The leading host is the first path element that holds "." or ":" (or is "localhost"). A
//     reference without one is on docker.io, and a bare "name" is docker.io/library/name. The names
//     index.docker.io and registry-1.docker.io count as docker.io. Host names are compared in lower case.
//   - mirrors[host] replaces that host when it has an entry. Otherwise registry replaces it when it
//     is set. Otherwise the reference is returned as it is.
//   - A reference that already starts with registry or a mirror value is returned as it is, so
//     nothing is prefixed twice.
//   - The path, the tag and the digest are kept. A digest still verifies through a pull-through
//     cache, and the node agent checks it, so the cache needs no trust.
//
// The result is part of the template hash: a changed mirror name re-hashes every template.
func RewriteImage(ref, registry string, mirrors map[string]string) string {
	targets := make([]string, 0, len(mirrors)+1)
	targets = append(targets, registry)
	for _, t := range mirrors {
		targets = append(targets, t)
	}
	for _, t := range targets {
		if t != "" && strings.HasPrefix(ref, strings.TrimSuffix(t, "/")+"/") {
			return ref
		}
	}
	parts := strings.Split(ref, "/")
	host, rest := "docker.io", ref
	switch {
	case len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || strings.EqualFold(parts[0], "localhost")):
		host = strings.ToLower(parts[0])
		if host == "index.docker.io" || host == "registry-1.docker.io" {
			host = "docker.io"
		}
		rest = strings.Join(parts[1:], "/")
		// Docker Hub has no API alias: docker.io/alpine is docker.io/library/alpine.
		if host == "docker.io" && !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	case len(parts) == 1:
		rest = "library/" + ref
	}
	target := mirrors[host]
	if target == "" {
		target = registry
	}
	if target == "" {
		return ref
	}
	return fmt.Sprintf("%s/%s", strings.TrimSuffix(target, "/"), rest)
}
