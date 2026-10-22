// SPDX-License-Identifier: Apache-2.0

package agentd

import "github.com/procoduck/koinon/internal/db/store"

// HarnessArgs exposes the launch command's harness switches, read from a raw version config,
// to the external test package.
func HarnessArgs(f store.AgentFlavor, config []byte) []string {
	c, _ := versionConfig(config)
	return harnessArgs(f, c)
}

// VersionModel exposes the model a raw version config names.
func VersionModel(config []byte) string {
	c, _ := versionConfig(config)
	return versionModel(c)
}

// Within exposes the permission policy's workspace check.
var Within = within

// LinkScript and InstructionLink expose the instruction link's restore script and paths.
var (
	LinkScript      = linkScript
	InstructionLink = instructionLink
)

// SweepScript exposes the script that ends the processes left in the sandbox.
var SweepScript = sweepScript
