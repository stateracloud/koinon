// SPDX-License-Identifier: Apache-2.0

package declarative_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/declarative"
)

// pagesDirEnv names the directory of docs pages that the two tests below check. The docs site lives in
// stateracloud/koinon-docs (ADR 0108); its CI sets this variable and runs these tests at a release tag.
const pagesDirEnv = "KOINON_PAGES_DIR"

// pageBlock is one fenced manifest block found in a page.
type pageBlock struct {
	where string // file and block number, for failure messages
	doc   string
}

// pagesDir returns the directory named by KOINON_PAGES_DIR, or skips the test when it is not set.
func pagesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(pagesDirEnv)
	if dir == "" {
		t.Skipf("%s is not set; stateracloud/koinon-docs sets it in its CI to the directory of its pages", pagesDirEnv)
	}
	return dir
}

// pageBlocks walks dir for .md and .mdx files and returns every block that matches re.
// It fails the test when dir does not exist or cannot be read.
func pageBlocks(t *testing.T, dir string, re *regexp.Regexp) []pageBlock {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("%s=%q: %v", pagesDirEnv, dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s=%q is not a directory", pagesDirEnv, dir)
	}
	var found []pageBlock
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".mdx")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		for i, m := range re.FindAllStringSubmatch(string(raw), -1) {
			found = append(found, pageBlock{where: fmt.Sprintf("%s (block %d)", rel, i+1), doc: m[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s=%q: %v", pagesDirEnv, dir, err)
	}
	return found
}

// TestDocumentedSecretsParse keeps every model-provider manifest the docs pages show valid for the
// importer, so a change to the Secret's format cannot leave one behind. It reads the pages in
// KOINON_PAGES_DIR and is skipped without it.
func TestDocumentedSecretsParse(t *testing.T) {
	dir := pagesDir(t)
	block := regexp.MustCompile("(?s)```yaml model-provider\n(.*?)```")
	found := pageBlocks(t, dir, block)
	for _, b := range found {
		sec := parseManifest(t, b.where, b.doc, declarative.TypeModelProvider)
		if _, err := declarative.ParseProvider(sec, "v"); err != nil {
			t.Errorf("%s: %v", b.where, err)
		}
	}
	if len(found) < 2 {
		t.Errorf("found %d yaml model-provider blocks in %s, want at least 2", len(found), dir)
	}
}

// TestDocumentedMCPServerSecretsParse keeps the mcp-server manifests in the docs pages valid. It reads
// the pages in KOINON_PAGES_DIR and is skipped without it.
func TestDocumentedMCPServerSecretsParse(t *testing.T) {
	dir := pagesDir(t)
	block := regexp.MustCompile("(?s)```yaml mcp-server\n(.*?)```")
	found := pageBlocks(t, dir, block)
	if len(found) == 0 {
		t.Fatalf("%s has no yaml mcp-server block", dir)
	}
	for _, b := range found {
		sec := parseManifest(t, b.where, b.doc, declarative.TypeMCPServer)
		if _, err := declarative.ParseMCPServer(sec, "v"); err != nil {
			t.Errorf("%s: %v", b.where, err)
		}
	}
}
