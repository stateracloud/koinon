// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"regexp"
	"strings"
)

// The backup and restore guide (test/e2e/commands/backup-restore.e1) takes the namespace and the release
// as the variables NS and REL (each block defaults them to koinon). The drill runs the blocks as written,
// with NS and REL set to the install under test, so the guide stays tested.

// GuideEnv is the environment that points a guide block at the install in ns with the given release.
// Release must contain "koinon" (Topology.Validate): the chart then names its objects <release>-api and so on.
func GuideEnv(ns, release string) ([]string, error) {
	if err := CheckNamespace(ns); err != nil {
		return nil, err
	}
	if !strings.Contains(release, "koinon") {
		return nil, fmt.Errorf("release %q must contain \"koinon\"", release)
	}
	return []string{"NS=" + ns, "REL=" + release}, nil
}

var hardcodedRE = []*regexp.Regexp{
	regexp.MustCompile(`(^|\s)-n koinon(\s|$)`),
	regexp.MustCompile(`\b(deploy|deployment)/koinon-(api|reconciler|agentd|gateway|pg|kek)\b`),
	regexp.MustCompile(`(^|[\s,'"])cnpg\.io/cluster=koinon-pg\b`),
	regexp.MustCompile(`(^|[\s,'"])app\.kubernetes\.io/instance=koinon([,'"\s]|$)`),
	regexp.MustCompile(`\bsecrets? koinon-(api|reconciler|agentd|gateway|pg|kek)\b`),
	regexp.MustCompile(`\b(svc|service|secret|statefulset|job|pod)/koinon(-(api|reconciler|agentd|gateway|pg|kek)\b|\s|$)`),
}

// Hardcoded reports a name of the guide's default install written literally in text instead of through
// $NS and $REL, so a new guide command that ignores the variables cannot run against the wrong install.
func Hardcoded(text string) error {
	for _, re := range hardcodedRE {
		if m := re.FindString(text); m != "" {
			return fmt.Errorf("the guide block names the default install literally (%q); use $NS and $REL", strings.TrimSpace(m))
		}
	}
	return nil
}

var secretNameRE = regexp.MustCompile(`(?m)^(  name: )(\S+)$`)

// RenameKEKSecret changes the name of the Secret in a `koinon-api keys export` manifest from -> to.
// The export names the Secret after the source release (<release>-kek); the target install, in
// the same cluster, has another release and so another Secret name. A real restore into a fresh
// namespace uses the same release name, so the guide has no such step; the drill needs it only
// because source and target share a cluster (and so its cluster-scoped chart objects).
func RenameKEKSecret(manifest, from, to string) (string, error) {
	found := 0
	out := secretNameRE.ReplaceAllStringFunc(manifest, func(line string) string {
		m := secretNameRE.FindStringSubmatch(line)
		if m[2] == from {
			found++
			return m[1] + to
		}
		return line
	})
	if found != 1 {
		return "", fmt.Errorf("the KEK export names the Secret %q %d times, want exactly once", from, found)
	}
	return out, nil
}
