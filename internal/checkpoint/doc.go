// SPDX-License-Identifier: Apache-2.0

// Package checkpoint saves an agent's home to object storage and restores it (plan item HC1;
// ADR 0032, practice B3).
//
// The home is read out of the sandbox as a tar stream over OpenShell exec, as saved work is read
// (ADR 0054), and written by [Writer] in koinon-agentd, which alone holds the storage credentials
// and the repository password. Nothing of either reaches a sandbox. The tool is kopia, embedded
// as a library: one repository per agent at orgs/<org>/homes/<agent>/ in one bucket, zstd
// compression, a random repository password sealed by the secret store (see [Passwords]).
//
// The tar stream comes from a sandbox the agent controls, so it is untrusted: [Stage] extracts it
// into a scratch directory through an [os.Root] (no path leaves the directory, symbolic links
// included), refuses absolute names, links to other files, devices and anything over the size
// cap, and drops excluded paths (see [Excluded]) even when the sandbox sent them.
package checkpoint
