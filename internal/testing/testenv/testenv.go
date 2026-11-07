// SPDX-License-Identifier: Apache-2.0

// Package testenv declares what a Ginkgo suite or spec needs from the machine it runs on (Docker,
// envtest assets, a variable that points at a live system) and decides what happens when it is
// missing (koinon-private#507).
//
// Without the requirement a test skips, so `make test` keeps working on a laptop without Docker.
// The skip message starts with Prefix and the requirement's name ("koinon:requires=docker: ..."),
// which hack/testverdict reads to report the suite as SKIPPED instead of PASS. When the name is
// listed in KOINON_REQUIRE (comma-separated, or "all"), the test fails instead: CI sets it, so a
// runner whose Docker broke cannot turn every Postgres suite green by skipping them.
//
// Call Require at the top of BeforeSuite (or of a spec, for a requirement only some specs have),
// never in the TestX function before RunSpecs: a suite that skips there under `ginkgo -p` exits
// before its processes report, and Ginkgo then prints "Test Suite Passed" with no report at all.
package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/testcontainers/testcontainers-go"
)

// Prefix starts every skip message Require writes; hack/testverdict matches on it.
const Prefix = "koinon:requires="

// RequireVar names the variable that turns a missing requirement from a skip into a failure.
const RequireVar = "KOINON_REQUIRE"

// Requirement is something a test needs from its environment. Name is a short lowercase word
// ("docker"), the token KOINON_REQUIRE lists and the skip message carries.
type Requirement struct {
	Name  string
	check func() error
}

// UnavailableVar lists requirements to treat as missing even when they are met (comma-separated,
// or "all"), so the path without Docker can be tested on a machine that has it. A dead DOCKER_HOST
// does not do that: testcontainers falls back to the default socket when DOCKER_HOST fails.
const UnavailableVar = "KOINON_TESTENV_UNAVAILABLE"

// Err is nil when the requirement is met, otherwise why it is not.
func (r Requirement) Err() error {
	if v := os.Getenv(UnavailableVar); Required(v, r.Name) {
		return fmt.Errorf("%s=%s marks it unavailable", UnavailableVar, v)
	}
	return r.check()
}

// Docker is a container runtime testcontainers can reach (the check is the one
// testcontainers.SkipIfProviderIsNotHealthy makes, done once per process).
var Docker = Requirement{Name: "docker", check: dockerErr}

// Envtest is KUBEBUILDER_ASSETS pointing at a directory of envtest binaries
// (`setup-envtest use -p path`).
var Envtest = Requirement{Name: "envtest", check: envtestErr}

// Var is a requirement met when the environment variable is set and not empty, for a suite that
// runs against a system someone else starts (the e2e install, an OpenShell gateway).
func Var(name, variable string) Requirement {
	return Requirement{Name: name, check: func() error {
		if os.Getenv(variable) == "" {
			return fmt.Errorf("%s is not set", variable)
		}
		return nil
	}}
}

// Vars is Var for a system named by several variables: one requirement name, met only when every
// variable is set, so KOINON_REQUIRE=<name> fails on a partial setup instead of skipping.
func Vars(name string, variables ...string) Requirement {
	return Requirement{Name: name, check: func() error {
		var missing []string
		for _, v := range variables {
			if os.Getenv(v) == "" {
				missing = append(missing, v)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s not set", strings.Join(missing, ", "))
		}
		return nil
	}}
}

// Binary is a requirement met when the executable is on PATH; its name is the executable's.
func Binary(bin string) Requirement { return BinaryNamed(bin, bin) }

// BinaryNamed is Binary with a requirement name of its own, for an executable whose path comes from
// a setting (name "helm", bin the value of $HELM).
func BinaryNamed(name, bin string) Requirement {
	return Requirement{Name: name, check: func() error {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s is not on PATH", bin)
		}
		return nil
	}}
}

// Require skips the current Ginkgo node when a requirement is missing, or fails it when
// KOINON_REQUIRE lists the requirement. In BeforeSuite that skips (or fails) the whole suite.
func Require(reqs ...Requirement) {
	ginkgo.GinkgoHelper()
	for _, r := range reqs {
		skip, msg := decide(r, os.Getenv(RequireVar))
		switch {
		case msg == "":
		case skip:
			ginkgo.Skip(msg)
		default:
			ginkgo.Fail(msg)
		}
	}
}

// RequireT is Require for a plain `testing` test: it skips t when a requirement is missing, or
// fails it when KOINON_REQUIRE lists the requirement.
func RequireT(t testing.TB, reqs ...Requirement) {
	t.Helper()
	for _, r := range reqs {
		skip, msg := decide(r, os.Getenv(RequireVar))
		switch {
		case msg == "":
		case skip:
			t.Skip(msg)
		default:
			t.Fatal(msg)
		}
	}
}

// decide returns "" when r is met; otherwise the message and whether to skip (true) or fail.
func decide(r Requirement, required string) (skip bool, msg string) {
	err := r.Err()
	if err == nil {
		return false, ""
	}
	msg = Message(r.Name, err)
	if Required(required, r.Name) {
		return false, msg + " (" + RequireVar + "=" + required + " makes this a failure)"
	}
	return true, msg
}

// Message is the skip message for a missing requirement: Prefix, the name, then why.
func Message(name string, err error) string { return Prefix + name + ": " + err.Error() }

// Required reports whether the KOINON_REQUIRE value lists name (or "all").
func Required(value, name string) bool {
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v == name || v == "all" {
			return true
		}
	}
	return false
}

var docker = sync.OnceValue(func() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("docker provider panicked: %v", r)
		}
	}()
	p, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Health(ctx); err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}
	return nil
})

func dockerErr() error { return docker() }

func envtestErr() error {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		return errors.New("KUBEBUILDER_ASSETS is not set (setup-envtest use -p path)")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() { // #nosec G703 -- only checks the test runner's own setting
		return fmt.Errorf("KUBEBUILDER_ASSETS=%s is not a directory", dir)
	}
	return nil
}
