// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The network enforcement mode a sandbox must report on the restored copy (ADR 0045).
const enforceMode = "enforce"

var enforcementRE = regexp.MustCompile(`\bEnforcement:(\S*)`)

// ParseExportEnforcement reads the sandbox enforcement mode out of the line `koinon-api run-evidence
// export` prints (internal/runevidence.Summary, "exported {... Enforcement:enforce ...} to DIR"). The
// drill reads the evidence through that command, the path the product and the lab use (#926), so a
// change in how the mode is recorded or exported shows here. An output without the field is an error,
// not "unknown": the drill must not read a silent export as a pass.
func ParseExportEnforcement(out string) (string, error) {
	m := enforcementRE.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("the export output names no Enforcement field: %q", out)
	}
	if m[1] == "" {
		return "", fmt.Errorf("the export output has an empty Enforcement field: %q", out)
	}
	return m[1], nil
}

// VerifyEnforcement is the verdict of the drill's enforcement check: the restored copy has at least one
// run, and every run reports enforce. modes maps a run ID to the mode its evidence export reported. Any
// other value (unknown, audit, none) and zero runs fail; the message names every run that is not enforce.
func VerifyEnforcement(modes map[string]string) error {
	if len(modes) == 0 {
		return fmt.Errorf("the restored copy has no run, so no sandbox enforcement was read")
	}
	var bad []string
	for id, m := range modes {
		if m != enforceMode {
			bad = append(bad, fmt.Sprintf("run %s reports %q", id, m))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("sandbox enforcement is not %q on every run (ADR 0045): %s", enforceMode, strings.Join(bad, "; "))
	}
	return nil
}
