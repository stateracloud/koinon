// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Credentials are temporary credentials as a test hands them to NewWithCredentials.
type Credentials = credentials

// NewWithCredentials is New with credentials from fetch instead of the pod's identity (tests:
// LocalStack accepts any), and plain http on any host (a container's host is not always the
// loopback).
func NewWithCredentials(cfg Config, fetch func(ctx context.Context) (Credentials, error)) (*Wrapper, error) {
	w, err := newWrapper(cfg, true)
	if err != nil {
		return nil, err
	}
	w.fetch = fetch
	return w, nil
}

// NewAnyHTTP is New with plain http on any host, for the endpoints and the credentials URI.
func NewAnyHTTP(cfg Config) (*Wrapper, error) {
	w, err := newWrapper(cfg, true)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Identity.RoleARN != "":
		w.fetch = w.webIdentity
	default:
		if err := validCredentialsURI(cfg.Identity.CredentialsURI, true); err != nil {
			return nil, err
		}
		w.fetch = w.podIdentity
	}
	return w, nil
}

// SetClock makes w read the time from now (credential expiry).
func (w *Wrapper) SetClock(now func() time.Time) { w.now = now }

// SetLogger sets where the adapter logs.
func (w *Wrapper) SetLogger(l *slog.Logger) { w.log = l }

// Sign is the SigV4 signer.
func Sign(req *http.Request, body []byte, accessKey, secretKey, token, region, service string, now time.Time) {
	sign(req, body, credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey, SessionToken: token}, region, service, now)
}

// SigningKey is the SigV4 key derivation.
var SigningKey = signingKey

// ErrDryRunUnverified is the probe's reason for an endpoint that does not do the dry run.
var ErrDryRunUnverified = errDryRunUnverified

// RefreshIdle reports whether no credential refresh is in flight: a refresh that was started has
// finished and installed its result (or kept the old credentials after a failure). Tests wait on
// it, on the client side, before they move the clock or the token again.
func (w *Wrapper) RefreshIdle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending == nil
}
