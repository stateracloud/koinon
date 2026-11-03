// SPDX-License-Identifier: Apache-2.0

package openbao

import (
	"context"
	"time"
)

// NewWithLogin is New with another sign-in than Kubernetes auth (tests: the dev server's tokens)
// and plain http on any host (a container's host is not always the loopback).
func NewWithLogin(cfg Config, login func(ctx context.Context) (string, time.Duration, error)) (*Wrapper, error) {
	w, err := newWrapper(cfg, true)
	if err != nil {
		return nil, err
	}
	w.login = login
	return w, nil
}

// NewAnyHTTP is New with plain http on any host.
func NewAnyHTTP(cfg Config) (*Wrapper, error) {
	w, err := newWrapper(cfg, true)
	if err != nil {
		return nil, err
	}
	w.login = w.kubernetesLogin
	return w, nil
}
