// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"regexp"
	"strings"
)

// The backup and restore guide (test/e2e/commands/backup-restore.e1) writes the namespace `koinon`
// and the release `koinon`. The drill runs the blocks as written, with those two names replaced, so
// the guide stays tested. This is an interim measure (ADR 0109, Consequences): its exit is the
// parameterised restore guide, after which the blocks take variables and Substitute goes away.
var (
	nsFlagRE   = regexp.MustCompile(`(^|\s)-n koinon(\s|$)`)
	objectRE   = regexp.MustCompile(`\b(deploy|deployment)/koinon-`)
	clusterRE  = regexp.MustCompile(`cnpg\.io/cluster=koinon-pg\b`)
	instanceRE = regexp.MustCompile(`app\.kubernetes\.io/instance=koinon([,'"\s]|$)`)
)

// Substitute rewrites one guide block for the install in namespace ns, release release. Release
// must contain "koinon" (Topology.Validate): the chart then names its objects <release>-api and so on.
// It fails when a name of the guide's `koinon` install is still in the text afterwards, so a new
// guide command that Substitute does not know cannot run against the wrong install.
func Substitute(block, ns, release string) (string, error) {
	if err := CheckNamespace(ns); err != nil {
		return "", err
	}
	if !strings.Contains(release, "koinon") {
		return "", fmt.Errorf("release %q must contain \"koinon\"", release)
	}
	out := block
	for nsFlagRE.MatchString(out) {
		out = nsFlagRE.ReplaceAllString(out, "${1}-n "+ns+"${2}")
	}
	out = objectRE.ReplaceAllString(out, "${1}/"+release+"-")
	out = clusterRE.ReplaceAllString(out, "cnpg.io/cluster="+release+"-pg")
	out = instanceRE.ReplaceAllString(out, "app.kubernetes.io/instance="+release+"${1}")
	if err := Unsubstituted(out); err != nil {
		return "", err
	}
	return out, nil
}

var leftoverRE = []*regexp.Regexp{
	regexp.MustCompile(`(^|\s)-n koinon(\s|$)`),
	regexp.MustCompile(`\b(deploy|deployment)/koinon-(api|reconciler|agentd|gateway|pg|kek)\b`),
	regexp.MustCompile(`cnpg\.io/cluster=koinon-pg\b`),
	regexp.MustCompile(`app\.kubernetes\.io/instance=koinon([,'"\s]|$)`),
	regexp.MustCompile(`\b(svc|service|secret|statefulset|job|pod)/koinon(-(api|reconciler|agentd|gateway|pg|kek)\b|\s|$)`),
}

// Unsubstituted reports a name of the guide's own install left in text.
func Unsubstituted(text string) error {
	for _, re := range leftoverRE {
		if m := re.FindString(text); m != "" {
			return fmt.Errorf("the guide block still names the guide's install (%q); teach drill.Substitute this command", strings.TrimSpace(m))
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
