// SPDX-License-Identifier: Apache-2.0

package agentd

// HarnessArgs exposes the launch command's harness switches to the external test package.
var HarnessArgs = harnessArgs

// Within exposes the permission policy's workspace check.
var Within = within

// LinkScript and InstructionLink expose the instruction link's restore script and paths.
var (
	LinkScript      = linkScript
	InstructionLink = instructionLink
)

// SweepScript exposes the script that ends the processes left in the sandbox.
var SweepScript = sweepScript
