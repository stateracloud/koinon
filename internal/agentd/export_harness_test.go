// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"fmt"

	"github.com/stateracloud/koinon/internal/acp"
)

// HarnessArgs exposes the launch command's harness switches, read from a raw version config,
// to the external test package.
func HarnessArgs(f string, config []byte) []string {
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

// RealpathScript is the script that resolves a permission request's paths in the sandbox.
const RealpathScript = realpathScript

// PathResolver resolves paths in the sandbox; Decide is the permission policy's answer.
type PathResolver = pathResolver

// Decide answers a permission request for a session in workspace.
func Decide(p *PermissionPolicy, req acp.PermissionRequest, workspace string, resolve PathResolver) (verdict Verdict, optionID, outside string) {
	d := p.decide(req, workspace, resolve)
	return d.Verdict, d.OptionID, d.Outside
}

// LinkScript and InstructionLink expose the instruction link's restore script and paths.
var (
	LinkScript      = linkScript
	WriteScript     = writeScript
	InstructionLink = instructionLink
)

// SweepScript exposes the script that ends the processes left in the sandbox.
var SweepScript = sweepScript

// ProcinfoScript and SaveworkScript expose the stuck-sandbox scripts (stuck.go, savework.go).
var (
	ProcinfoScript = procinfoScript
	SaveworkScript = saveworkScript
)

// Handoff is the handoff of a fresh session, for the external test package.
type Handoff = handoff

// HandoffMaxBytes and SummaryMaxBytes are the size caps.
const (
	HandoffMaxBytes = handoffMaxBytes
	SummaryMaxBytes = summaryMaxBytes
)

// Build renders the handoff.
func (h Handoff) Build() string { return h.build() }

// TruncateSummary cuts a summary to its cap.
var TruncateSummary = truncateSummary

// ParseSaveOutput reads savework.sh's output.
var ParseSaveOutput = parseSaveOutput

// SurvivorsOf and ProcinfoOf read the sweep's and procinfo's output lines.
func SurvivorsOf(out string) []string {
	var r []string
	for _, s := range parseSurvivors(out) {
		r = append(r, s.String())
	}
	return r
}

// ProcinfoOf returns the processes procinfo.sh printed as "pid|state|comm|wchan".
func ProcinfoOf(out string) []string {
	var r []string
	for _, p := range parseProcinfo(out) {
		r = append(r, fmt.Sprintf("%d|%s|%s|%s", p.PID, p.State, p.Comm, p.Wchan))
	}
	return r
}

// CapNotes bounds the notes a sandbox reports.
var CapNotes = capNotes
