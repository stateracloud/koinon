// SPDX-License-Identifier: Apache-2.0

package project_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/project"
)

func TestModulePathMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	if got := strings.TrimPrefix(first, "module "); got != project.ModulePath {
		t.Fatalf("go.mod module %q, project.ModulePath %q", got, project.ModulePath)
	}
}

func TestGroupsUnderDomain(t *testing.T) {
	for _, group := range []string{project.AgentsGroup, project.TemplatesGroup} {
		if !strings.HasSuffix(group, "."+project.Domain) {
			t.Fatalf("%q is not under %q", group, project.Domain)
		}
	}
}

func TestDocsURLs(t *testing.T) {
	for _, u := range []string{project.SecretKeysDocURL, project.InstallAuditDocURL} {
		if !strings.HasPrefix(u, project.DocsURL+"/docs/") {
			t.Fatalf("%q is not a page of the docs site %q", u, project.DocsURL)
		}
	}
}
