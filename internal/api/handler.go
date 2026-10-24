// SPDX-License-Identifier: Apache-2.0

// Package api assembles koinon-api's HTTP handler: connect-rpc services
// behind the auth interceptor, the login endpoints and CSRF protection.
package api

import (
	"fmt"
	"io/fs"
	"net/http"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/stateracloud/koinon/internal/auth"
)

// Options configures NewHandler.
type Options struct {
	Auth *auth.Service
	// TrustedOrigins are extra origins (scheme://host[:port]) allowed to
	// make state-changing requests, besides the request's own host. Needed
	// only when the SPA is served from another origin, such as a dev server.
	TrustedOrigins []string
	// Services registers further connect services behind the auth
	// interceptor.
	Services []func(*connect.Server)
	// Mounts register further handlers on the mux, outside the session-authenticated
	// connect-rpc API (the admin API under /admin/v1 authenticates bearer tokens itself).
	Mounts []func(*http.ServeMux)
	// UI is the built web UI (web.Dist()), served for every path no other
	// handler claims; nil serves none.
	UI fs.FS
}

// NewHandler returns the API's HTTP handler.
//
// CSRF protection for cookie sessions has two layers (ADR 0022):
//
//  1. Every non-safe request (anything but GET, HEAD, OPTIONS) must come from
//     this origin: net/http's CrossOriginProtection rejects it when the
//     browser's Sec-Fetch-Site says cross-site, or, for older browsers, when
//     Origin names another host. This covers the /auth/* form endpoints.
//  2. Connect unary RPCs must carry the Connect-Protocol-Version header,
//     which connect-web always sends. A cross-site page cannot add a custom
//     header without a CORS preflight, and the API answers no preflights. gRPC,
//     gRPC-Web and Connect streaming use non-form content types, which need a
//     preflight too.
//
// The session cookie is also SameSite=Lax, so browsers do not send it on
// cross-site subrequests at all.
func NewHandler(opts Options) (http.Handler, error) {
	server := connect.NewServer(opts.Auth.Interceptor())
	opts.Auth.Register(server)
	for _, register := range opts.Services {
		register(server)
	}

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithRequireConnectProtocolHeader())
	opts.Auth.Mount(mux)
	for _, mount := range opts.Mounts {
		mount(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if opts.UI != nil {
		mux.Handle("/", spaHandler(opts.UI))
	}

	cop := http.NewCrossOriginProtection()
	for _, o := range opts.TrustedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("trusted origin %q: %w", o, err)
		}
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	return cop.Handler(mux), nil
}
