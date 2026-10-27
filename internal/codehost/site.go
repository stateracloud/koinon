// SPDX-License-Identifier: Apache-2.0

package codehost

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stateracloud/koinon/connectors"
)

// MaxCABundleBytes bounds a connection's CA bundle.
const MaxCABundleBytes = 256 << 10

// Site is where a connection's code host lives (ADR 0062): a base URL and an optional CA
// bundle, stored in the non-secret config of the connection under baseUrl and caBundle. Both
// are empty for a connection on the connector's default host. A CA bundle is public, so it is
// stored and delivered like any other config.
type Site struct {
	// BaseURL is the host's web base URL, https://host[:port].
	BaseURL string `json:"baseUrl,omitempty"`
	// CABundle is PEM certificates that sign the host's TLS certificate, besides the system
	// roots.
	CABundle string `json:"caBundle,omitempty"`
}

// SiteOf decodes the site of a connection's config.
func SiteOf(config []byte) (Site, error) {
	var s Site
	if len(config) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(config, &s); err != nil {
		return Site{}, fmt.Errorf("codehost: decode connection config: %w", err)
	}
	return s, nil
}

// Location resolves the base URL against the connector type.
func (s Site) Location(t *connectors.Type) (connectors.Location, error) {
	return t.Locate(s.BaseURL)
}

// Pool returns the system roots plus the connection's CA bundle; nil when there is no bundle
// (use the transport's defaults).
func (s Site) Pool() (*x509.CertPool, error) {
	if strings.TrimSpace(s.CABundle) == "" {
		return nil, nil
	}
	return CertPool(s.CABundle)
}

// CertPool parses a PEM bundle into the system roots plus its certificates. It fails when the
// bundle holds no certificate, or anything else than PEM certificates.
func CertPool(bundle string) (*x509.CertPool, error) {
	if len(bundle) > MaxCABundleBytes {
		return nil, fmt.Errorf("the CA bundle is larger than %d KiB", MaxCABundleBytes>>10)
	}
	if err := checkPEM(bundle); err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(bundle)) {
		return nil, errors.New("the CA bundle holds no PEM certificate")
	}
	return pool, nil
}

// checkPEM rejects a bundle with a block that is not a certificate, which could be a private key
// pasted by mistake: the bundle is stored as public config and copied into sandboxes.
func checkPEM(bundle string) error {
	rest := []byte(bundle)
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("the CA bundle holds a %q block; only CERTIFICATE blocks are allowed", block.Type)
		}
		n++
	}
	if n == 0 {
		return errors.New("the CA bundle holds no PEM certificate")
	}
	return nil
}

// CapabilitiesOf builds the capability descriptor of a code-host connector type from its data.
func CapabilitiesOf(t *connectors.Type) (Capabilities, error) {
	c := t.Spec.Capabilities
	if c == nil {
		return Capabilities{}, fmt.Errorf("codehost: the %s connector type has no capability descriptor", t.Metadata.ID)
	}
	out := Capabilities{
		DisplayName:         c.DisplayName,
		CredentialClass:     c.CredentialClass,
		NarrowsRepositories: c.NarrowsRepositories,
		NarrowsPermissions:  c.NarrowsPermissions,
		MaxRepositories:     c.MaxRepositories,
		CanDenyCI:           c.CanDenyCI,
		ExactModes:          c.ExactModes,
		CIPaths:             append([]string(nil), t.Spec.CIPaths...),
	}
	if l := t.Spec.Auth.TokenLifetime; l != "" {
		d, err := time.ParseDuration(l)
		if err != nil {
			return Capabilities{}, fmt.Errorf("codehost: %s token lifetime: %w", t.Metadata.ID, err)
		}
		out.TokenLifetime = d
	}
	return out, nil
}
