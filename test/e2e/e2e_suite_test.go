// SPDX-License-Identifier: Apache-2.0

// Package e2e is the quickstart end-to-end test on kind (plan item E1, ADR 0049). It drives an
// installed Koinon through its public API exactly as the quickstart does: bootstrap login,
// GitHub connection, repo-helper agent, task, approval, pull request. The model is the fake
// model server and GitHub is the fake GitHub App API; nothing external is called.
//
// The cluster, images and install come from test/e2e/run.sh, which port-forwards the services
// and runs this suite with:
//
//	KOINON_E2E_API_URL         koinon-api, e.g. http://127.0.0.1:8080
//	KOINON_E2E_FAKEGITHUB_URL  the fake GitHub, e.g. http://127.0.0.1:18090
//	KOINON_E2E_FAKEMODEL_URL   the fake model server, e.g. http://127.0.0.1:18092
//	KOINON_E2E_PASSWORD        the bootstrap admin's one-time password
//	KOINON_E2E_T0              optional: Unix time `helm install` started, for the total and the 20-minute bound
//	KOINON_E2E_REPORT          optional: file to write the step timings to (Markdown)
//
// Without KOINON_E2E_API_URL the suite is skipped, so `go test ./...` stays offline.
package e2e_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Quickstart e2e (kind)")
}

var _ = BeforeSuite(func() {
	testenv.Require(testenv.Var("e2e", "KOINON_E2E_API_URL"))
})
