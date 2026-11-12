// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"net/http"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
)

// limiter slows down clients that keep presenting bad tokens. It is the limiter of the password login
// too (package ratelimit has the limits): one implementation for both credential entry points.
type limiter = ratelimit.Limiter

func newLimiter() *limiter { return ratelimit.New() }

// limiterKey is the slot of a client IP and a token prefix; "" is the shared slot of bad tokens that
// name no token.
func limiterKey(ip, prefix string) string { return ratelimit.Key(ip, prefix) }

func clientIP(r *http.Request) string { return ratelimit.ClientAddr(r) }
