// SPDX-License-Identifier: Apache-2.0

// Package project holds identifiers that must be defined exactly once, so a
// repository or domain move is a single search-and-replace (ADR 0034).
package project

const (
	// Name is the project name.
	Name = "koinon"

	// ModulePath is the Go module path. Keep in sync with go.mod and the
	// MODULE variable in the Makefile.
	ModulePath = "github.com/procoduck/koinon"

	// Domain is the project domain. Kubernetes API groups live under it.
	Domain = "koinon.dev"

	// AgentsGroup is the API group for agent resources.
	AgentsGroup = "agents." + Domain
)

// Version is the release version, set at build time with
// -ldflags "-X github.com/procoduck/koinon/internal/project.Version=...".
var Version = "dev"
