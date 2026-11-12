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

func TestGuideEnvSetsNamespaceAndRelease(t *testing.T) {
	env, err := drill.GuideEnv("koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env, " ") != "NS=koinon-drill-dst-a1b2c3 REL=koinon-drill-dst-a1b2c3" {
		t.Errorf("env = %v", env)
	}
}

func TestGuideEnvRefusesANamespaceOutsideThePrefixAndAForeignRelease(t *testing.T) {
	if _, err := drill.GuideEnv("koinon", "koinon-x"); err == nil {
		t.Fatal("the guide's own namespace must be refused")
	}
	if _, err := drill.GuideEnv("koinon-drill-src-a1b2c3", "prod"); err == nil {
		t.Fatal("a release without koinon in its name must be refused")
	}
}

func TestHardcodedFindsALiteralNameOfTheDefaultInstall(t *testing.T) {
	for _, bad := range []string{
		"kubectl -n koinon get pods\n",
		"kubectl get deploy/koinon-api\n",
		"kubectl get secret koinon-kek\n",
		"kubectl get secret/koinon-kek\n",
		"kubectl get pod -l cnpg.io/cluster=koinon-pg\n",
	} {
		if err := drill.Hardcoded(bad); err == nil {
			t.Errorf("%q must be reported", bad)
		}
	}
	if err := drill.Hardcoded("kubectl -n \"$NS\" exec deploy/\"$REL\"-api -- koinon-api keys status\n"); err != nil {
		t.Errorf("the binary's name is not an install name: %v", err)
	}
}

// Every block of the guide takes the namespace and the release from the variables: a new guide command
// that writes the default install's names fails here, offline, instead of in the nightly run.
func TestEveryBlockOfTheGuideUsesTheVariables(t *testing.T) {
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
		if err := drill.Hardcoded(string(text)); err != nil {
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

func TestGuideSourceEnvAddsTheSourceAndRefusesARealNamespace(t *testing.T) {
	env, err := drill.GuideSourceEnv("koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3", "koinon-drill-src-a1b2c3", "koinon-drill-src-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	want := "NS=koinon-drill-dst-a1b2c3 REL=koinon-drill-dst-a1b2c3 SRC_NS=koinon-drill-src-a1b2c3 SRC_REL=koinon-drill-src-a1b2c3"
	if strings.Join(env, " ") != want {
		t.Errorf("env = %v", env)
	}
	if _, err := drill.GuideSourceEnv("koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3", "koinon", "koinon"); err == nil {
		t.Fatal("a source outside the drill prefix must be refused")
	}
	if _, err := drill.GuideSourceEnv("koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3", "koinon-drill-src-a1b2c3"); err == nil {
		t.Fatal("a source in the target's namespace must be refused")
	}
	if _, err := drill.GuideSourceEnv("koinon-drill-dst-a1b2c3", "koinon-drill-dst-a1b2c3", "koinon-drill-src-a1b2c3", "prod"); err == nil {
		t.Fatal("a source release without koinon in its name must be refused")
	}
}
