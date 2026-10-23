// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The rule (ADRs 0001, 0049): every query that touches an org-owned table
// takes the @org_id parameter, and every read or write of an org-owned table
// is constrained by an org_id predicate, either against @org_id or, for a
// join, against another org-scoped table's org_id.
//
// The check is textual on purpose: it counts references to org-owned tables
// after FROM, JOIN and UPDATE, and requires at least as many org_id
// predicates. It does not prove that each predicate belongs to the right
// table, but it fails on the common mistakes (a missing filter, a positional
// $1 instead of @org_id, a join that drops the org). Org-owned tables are
// those whose CREATE TABLE in the migrations has an org_id column.

var (
	createTableRE = regexp.MustCompile(`(?is)create table (\w+) \((.*?)\n\);`)
	orgColumnRE   = regexp.MustCompile(`(?im)^\s*org_id\s`)
	queryNameRE   = regexp.MustCompile(`(?m)^-- name: (\w+)`)
	commentRE     = regexp.MustCompile(`--[^\n]*`)
	tableRefRE    = regexp.MustCompile(`(?i)\b(from|join|update|into)\s+(\w+)`)
	orgPredRE     = regexp.MustCompile(`(?i)\b(?:\w+\.)?org_id\s*=\s*(?:@org_id\b|(?:\w+\.)?org_id\b)`)
	orgParamRE    = regexp.MustCompile(`@org_id\b`)
)

// crossOrgQueries lists the queries that deliberately span orgs and so are
// exempt from the org_id rule, keyed by "<file>:<query>". Each entry needs a
// one-line reason and a review in the PR that adds it; keep the list to
// queries that act for a user across all of their orgs, before any org is
// chosen. The test fails if an entry is stale (missing, or no longer flagged).
var crossOrgQueries = map[string]string{
	"auth.sql:UpsertGroupMemberships":      "login: grants the user's IdP-group roles in every org mapped to those groups",
	"auth.sql:DeleteStaleGroupMemberships": "login: revokes the user's IdP-group roles in every org whose group they lost",
	"auth.sql:ListEffectiveMemberships":    "session: lists the orgs the user belongs to, to pick one; scoped by user_id",
}

func TestQueriesAreOrgScoped(t *testing.T) {
	owned := orgOwnedTables(t, "../migrations")
	for _, table := range []string{"agents", "agent_versions", "tickets", "ticket_events", "runs", "run_events"} {
		if !owned[table] {
			t.Fatalf("expected %s to be detected as org-owned; detection is broken", table)
		}
	}
	if owned["templates"] || owned["orgs"] {
		t.Fatal("templates and orgs are not org-owned; detection is broken")
	}

	files, err := filepath.Glob("../queries/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no query files found")
	}
	checked := 0
	exempted := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- fixed test paths
		if err != nil {
			t.Fatal(err)
		}
		for name, sql := range splitQueries(string(data)) {
			key := filepath.Base(f) + ":" + name
			problem := orgScopeProblem(sql, owned)
			if _, ok := crossOrgQueries[key]; ok {
				if problem == "" {
					t.Errorf("%s is allowlisted as cross-org but passes the org_id check; remove it from crossOrgQueries", key)
				}
				exempted[key] = true
			} else if problem != "" {
				t.Errorf("%s: %s: %s", filepath.Base(f), name, problem)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no queries found")
	}
	for key, reason := range crossOrgQueries {
		if reason == "" {
			t.Errorf("%s: allowlisted without a reason", key)
		}
		if !exempted[key] {
			t.Errorf("%s is allowlisted in crossOrgQueries but no such query exists", key)
		}
	}
}

// TestOrgScopeCheckCatchesMistakes keeps the checker honest.
func TestOrgScopeCheckCatchesMistakes(t *testing.T) {
	owned := map[string]bool{"runs": true, "agents": true}
	cases := map[string]bool{ // sql -> want a problem
		`SELECT * FROM runs WHERE org_id = @org_id AND id = @id`:                                                   false,
		`SELECT * FROM runs WHERE id = @id`:                                                                        true,
		`SELECT * FROM runs WHERE org_id = $1 AND id = $2`:                                                         true,
		`INSERT INTO runs (org_id, state) VALUES (@org_id, 'queued')`:                                              false,
		`INSERT INTO runs (org_id, state) VALUES ($1, 'queued')`:                                                   true,
		`SELECT * FROM runs r JOIN agents a ON a.id = r.agent_id WHERE r.org_id = @org_id`:                         true,
		`SELECT * FROM runs r JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id WHERE r.org_id = @org_id`: false,
		`UPDATE runs SET state = 'canceled' WHERE id = @id -- org_id = @org_id`:                                    true,
		`SELECT * FROM templates WHERE id = $1`:                                                                    false,
	}
	for sql, want := range cases {
		if got := orgScopeProblem(sql, owned) != ""; got != want {
			t.Errorf("orgScopeProblem(%q) reported=%v, want %v", sql, got, want)
		}
	}
}

func orgOwnedTables(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	owned := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- fixed test paths
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTableRE.FindAllStringSubmatch(string(data), -1) {
			if orgColumnRE.MatchString(m[2]) {
				owned[strings.ToLower(m[1])] = true
			}
		}
	}
	return owned
}

// splitQueries maps sqlc query names to their SQL text.
func splitQueries(src string) map[string]string {
	out := map[string]string{}
	locs := queryNameRE.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[src[loc[2]:loc[3]]] = src[loc[1]:end]
	}
	return out
}

func orgScopeProblem(sql string, owned map[string]bool) string {
	sql = commentRE.ReplaceAllString(sql, "")
	refs, touches := 0, false
	for _, m := range tableRefRE.FindAllStringSubmatch(sql, -1) {
		if !owned[strings.ToLower(m[2])] {
			continue
		}
		touches = true
		if !strings.EqualFold(m[1], "into") {
			refs++
		}
	}
	if !touches {
		return ""
	}
	if !orgParamRE.MatchString(sql) {
		return "touches an org-owned table but does not take @org_id"
	}
	if preds := len(orgPredRE.FindAllString(sql, -1)); preds < refs {
		return "has fewer org_id predicates than references to org-owned tables"
	}
	return ""
}
