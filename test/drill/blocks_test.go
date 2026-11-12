// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/test/drill"
)

func TestSubstituteRewritesNamespaceAndRelease(t *testing.T) {
	in := "kubectl -n koinon scale deployment/koinon-reconciler deployment/koinon-api --replicas=1\n" +
		"kubectl -n koinon exec deploy/koinon-api -- koinon-api keys status\n" +
		"kubectl -n koinon get pod -l cnpg.io/cluster=koinon-pg,cnpg.io/instanceRole=primary -o name\n" +
		"kubectl -n koinon wait --for=delete pod -l 'app.kubernetes.io/instance=koinon,app.kubernetes.io/component in (api,reconciler)'\n"
	got, err := drill.Substitute(in, "koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-n koinon-drill-dst-a1b2c3 scale deployment/koinon-drill-dst-a1b2c3-reconciler deployment/koinon-drill-dst-a1b2c3-api",
		"exec deploy/koinon-drill-dst-a1b2c3-api -- koinon-api keys status", // the binary in the pod keeps its name
		"cnpg.io/cluster=koinon-drill-dst-a1b2c3-pg,",
		"app.kubernetes.io/instance=koinon-drill-dst-a1b2c3,app.kubernetes.io/component",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-n koinon ") {
		t.Errorf("namespace left in:\n%s", got)
	}
}

func TestSubstituteRefusesAnUnknownNameOfTheGuideInstall(t *testing.T) {
	_, err := drill.Substitute("kubectl -n koinon get svc/koinon-api\n", "koinon-drill-src-a1b2c3", "koinon-drill-src-a1b2c3")
	if err == nil {
		t.Fatal("a command Substitute does not know must be refused")
	}
}

func TestSubstituteRewritesSecretsAndRefusesTheSpacedForm(t *testing.T) {
	out, err := drill.Substitute("kubectl -n koinon get secret/koinon-kek\n", "koinon-drill-src-a1b2c3", "koinon-drill-src-a1b2c3")
	if err != nil || !strings.Contains(out, "secret/koinon-drill-src-a1b2c3-kek") {
		t.Fatalf("secret/koinon-kek must be rewritten: %q, %v", out, err)
	}
	if _, err := drill.Substitute("kubectl -n koinon get secret koinon-kek\n", "koinon-drill-src-a1b2c3", "koinon-drill-src-a1b2c3"); err == nil {
		t.Fatal("the spaced form `secret koinon-kek` must be refused")
	}
}

func TestSubstituteRefusesANamespaceOutsideThePrefix(t *testing.T) {
	if _, err := drill.Substitute("kubectl -n koinon get pods\n", "koinon", "koinon-x"); err == nil {
		t.Fatal("the guide's own namespace must be refused")
	}
	if _, err := drill.Substitute("kubectl -n koinon get pods\n", "koinon-drill-src-a1b2c3", "prod"); err == nil {
		t.Fatal("a release without koinon in its name must be refused")
	}
}

// Every block of the guide is rewritten without a leftover: a new guide command that the
// substitution does not cover fails here, offline, instead of in the nightly run.
func TestSubstituteCoversEveryBlockOfTheGuide(t *testing.T) {
	script := "../../hack/docs-e1.sh"
	if _, err := os.Stat(script); err != nil {
		t.Skip("run from the repository")
	}
	cmd := exec.CommandContext(context.Background(), script, "names")
	cmd.Env = append(os.Environ(), "E1_FILE=../e2e/commands/backup-restore.e1")
	names, err := cmd.Output()
	if err != nil {
		t.Fatalf("docs-e1.sh names: %v", err)
	}
	fields := strings.Fields(string(names))
	if len(fields) == 0 {
		t.Fatal("the guide has no blocks")
	}
	for _, n := range fields {
		show := exec.CommandContext(context.Background(), script, "show", n)
		show.Env = append(os.Environ(), "E1_FILE=../e2e/commands/backup-restore.e1")
		text, err := show.Output()
		if err != nil {
			t.Fatalf("docs-e1.sh show %s: %v", n, err)
		}
		if _, err := drill.Substitute(string(text), "koinon-drill-src-a1b2c3", "koinon-drill-src-a1b2c3"); err != nil {
			t.Errorf("block %s: %v", n, err)
		}
	}
}

func TestRenameKEKSecret(t *testing.T) {
	m := "# Koinon KEK export\napiVersion: v1\nkind: Secret\nmetadata:\n  labels:\n    koinon.dev/managed-by: koinon-api\n  name: koinon-drill-src-a1b2c3-kek\ntype: Opaque\n"
	got, err := drill.RenameKEKSecret(m, "koinon-drill-src-a1b2c3-kek", "koinon-drill-dst-a1b2c3-kek")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "  name: koinon-drill-dst-a1b2c3-kek\n") || strings.Contains(got, "src") {
		t.Errorf("not renamed:\n%s", got)
	}
	if _, err := drill.RenameKEKSecret(m, "other-kek", "x"); err == nil {
		t.Error("a missing Secret name must be an error")
	}
}
