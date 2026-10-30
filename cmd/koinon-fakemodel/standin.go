// SPDX-License-Identifier: Apache-2.0

package main

import "net/http"

// Standin mode (-standin) serves no model at all: every request, whatever its method, path or
// body, is answered with an empty 418 and the marker header. The escape suite points a sandbox
// at it as a stand-in for agentgateway, so that a request the sandbox policy allows is
// recognisable by its answer (418 and the marker) from one the policy denies (403 from the
// proxy) and from one that never reached the proxy (ADR 0080).
const (
	standinStatus      = http.StatusTeapot
	standinMarkerKey   = "X-Koinon-Standin"
	standinMarkerValue = "1"
)

func standinHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(standinMarkerKey, standinMarkerValue)
		w.WriteHeader(standinStatus)
	})
}
