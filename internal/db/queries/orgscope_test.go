// SPDX-License-Identifier: Apache-2.0

// Package queries_test checks the sqlc queries in this directory. It needs no
// database, so it runs without Docker.
package queries_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestQueries(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Queries Suite")
}

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
	"auth.sql:UpsertGroupMemberships":                   "login: grants the user's IdP-group roles in every org mapped to those groups",
	"auth.sql:DeleteStaleGroupMemberships":              "login: revokes the user's IdP-group roles in every org whose group they lost",
	"api_tokens.sql:GetAPITokenByPrefix":                "admin API: authenticates a bearer token, which names its org only after it is looked up",
	"connections.sql:ListConnectionsToMigrate":          "startup: moves every org's connection secrets from Kubernetes Secrets into the store",
	"connections.sql:ListOrgsWithCodeHostConnections":   "the code-host scope refresh: finds every org that has a live code-host connection, to re-read its repositories' visibility",
	"budget.sql:ListOrgBudgetSummaries":                 "platform admins: lists every org with its cap, ceiling and spend",
	"catalog.sql:ListModelsToRederiveFormat":            "startup: finds the models whose format follows the table, in every org, to re-derive them (ADR 0069)",
	"platform_prices.sql:ListPriceModelNames":           "the price refresh: the model names of every org, no org, provider or secret, to diff a public price source against the catalog (ADR 0098)",
	"platform_prices.sql:ProviderTypeInUse":             "the price refresh's auto mode: whether any org has a provider of a type, a yes or no only (ADR 0098)",
	"platform_prices.sql:ListModelsOfProviderType":      "the unpriced-model count: resolves every org's models of a provider type against that org's prices, reported as a count only (ADR 0098)",
	"prices.sql:ListPendingMigratedPrices":              "startup: finds the prices migration 00014 made from model rates, in every org, to complete them",
	"orgs.sql:ListOrgIDsWithRunWork":                    "the agentd claim loop: finds the live orgs with a claimable or expired-lease run, returning org ids only; every claim query after it is scoped by @org_id",
	"questions.sql:ListOrgsWithQuestionsToNotify":       "the question notifier: finds every org that has an opened question not yet announced",
	"questions.sql:ListOrgsWithOpenQuestions":           "the question sweep: finds every org that has an open question, to retarget the orphaned ones",
	"questions.sql:ListOrgsWithDueQuestions":            "the question timers: finds every org that has an open question with a reminder or an expiry due",
	"questions.sql:ListOrgsWithQuestionsOfFinishedRuns": "the question sweep: finds every org that has a question whose run ended",
	"spend.sql:ListOrgsWithDueSettlements":              "the spend settler: finds every org with a provisional entry due for an attempt",
	"cost_reconciliation.sql:ListCostReportProviders":   "the daily cost reconciliation: finds every provider with a cost API key",
	"audit.sql:ListAuditEventsAllOrgs":                  "platform admins: reads the audit log across every org",
	"audit.sql:ListAuditChainOrgs":                      "the audit verifier: lists every org that has a chain",
	"audit.sql:ListAuditChainHeads":                     "the audit anchor job: reads every org's chain head to commit to them in the install chain (ADR 0100 decision 4.1)",
	"audit.sql:ListInstallAnchorHeadsInRange":           "the install export: the org heads each anchor commits to as stream lines; install_audit_anchor_heads.org_id names the anchored org, no org owns the rows",
	"audit.sql:CheckAnchoredHeads":                      "the audit verifier: compares one anchor's org heads with each org chain (ADR 0100 decision 4.1)",
	"sandboxes.sql:CountAgentSandboxesByState":          "reconciler metrics: counts sandboxes by state across every org; returns counts only, no row or ID",
	"sandboxes.sql:ListAgentsForUnmarked":               "koinon-reconciler unmarked list (operator, read-only): each live org's agents with status and flavor, to give every unmarked OpenShell object of the install a reason",
	"sandboxes.sql:ListOrgWorkspacesForUnmarked":        "koinon-reconciler unmarked list (operator, read-only): which org of this database owns each OpenShell workspace of the install",
	"agents.sql:ListActiveAgentVersionsAllOrgs":         "the trigger scheduler: lists the active triggers of every live org's active agents; read-only",
	"agents.sql:ListAgentActivationsAllOrgs":            "the schedule trigger: when each live org's active agent version was activated; read-only",
	"trace.sql:LowerOrgTraceLevels":                     "platform ceiling: lowers every org above the new ceiling in one statement; returns the changed orgs for the audit log",
	"auth.sql:ListEffectiveMemberships":                 "session: lists the orgs the user belongs to, to pick one; scoped by user_id",
}

var _ = Describe("org scoping", func() {
	It("scopes every query on an org-owned table by @org_id", func() {
		owned := orgOwnedTables("../migrations")
		for _, table := range []string{"agents", "agent_versions", "tasks", "task_events", "runs", "run_events"} {
			Expect(owned).To(HaveKey(table), "expected %s to be detected as org-owned; detection is broken", table)
		}
		Expect(owned).NotTo(HaveKey("templates"), "templates is not org-owned; detection is broken")
		Expect(owned).NotTo(HaveKey("orgs"), "orgs is not org-owned; detection is broken")

		files, err := filepath.Glob("*.sql")
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty(), "no query files found")

		var problems []string
		checked := 0
		exempted := map[string]bool{}
		for _, f := range files {
			data, err := os.ReadFile(f) // #nosec G304 -- fixed test paths
			Expect(err).NotTo(HaveOccurred())
			for name, sql := range splitQueries(string(data)) {
				key := filepath.Base(f) + ":" + name
				problem := orgScopeProblem(sql, owned)
				if _, ok := crossOrgQueries[key]; ok {
					if problem == "" {
						problems = append(problems, fmt.Sprintf(
							"%s is allowlisted as cross-org but passes the org_id check; remove it from crossOrgQueries", key))
					}
					exempted[key] = true
				} else if problem != "" {
					problems = append(problems, fmt.Sprintf("%s: %s: %s", filepath.Base(f), name, problem))
				}
				checked++
			}
		}
		Expect(checked).NotTo(BeZero(), "no queries found")
		for key, reason := range crossOrgQueries {
			if reason == "" {
				problems = append(problems, key+": allowlisted without a reason")
			}
			if !exempted[key] {
				problems = append(problems, key+" is allowlisted in crossOrgQueries but no such query exists")
			}
		}
		sort.Strings(problems)
		Expect(problems).To(BeEmpty())
	})

	// Keeps the checker honest.
	DescribeTable("catches mistakes", func(sql string, wantProblem bool) {
		owned := map[string]bool{"runs": true, "agents": true}
		Expect(orgScopeProblem(sql, owned) != "").To(Equal(wantProblem))
	},
		Entry(nil, `SELECT * FROM runs WHERE org_id = @org_id AND id = @id`, false),
		Entry(nil, `SELECT * FROM runs WHERE id = @id`, true),
		Entry(nil, `SELECT * FROM runs WHERE org_id = $1 AND id = $2`, true),
		Entry(nil, `INSERT INTO runs (org_id, state) VALUES (@org_id, 'queued')`, false),
		Entry(nil, `INSERT INTO runs (org_id, state) VALUES ($1, 'queued')`, true),
		Entry(nil, `SELECT * FROM runs r JOIN agents a ON a.id = r.agent_id WHERE r.org_id = @org_id`, true),
		Entry(nil, `SELECT * FROM runs r JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id WHERE r.org_id = @org_id`, false),
		Entry(nil, `UPDATE runs SET state = 'canceled' WHERE id = @id -- org_id = @org_id`, true),
		Entry(nil, `SELECT * FROM templates WHERE id = $1`, false),
	)
})

var renameTableRE = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+RENAME\s+TO\s+(\w+)`)

func orgOwnedTables(dir string) map[string]bool {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	Expect(err).NotTo(HaveOccurred())
	sort.Strings(files)
	owned := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- fixed test paths
		Expect(err).NotTo(HaveOccurred())
		for _, m := range createTableRE.FindAllStringSubmatch(string(data), -1) {
			if orgColumnRE.MatchString(m[2]) {
				owned[strings.ToLower(m[1])] = true
			}
		}
		// A renamed table keeps its owner column (migration 00020 renames tickets to tasks).
		for _, m := range renameTableRE.FindAllStringSubmatch(string(data), -1) {
			if owned[strings.ToLower(m[1])] {
				delete(owned, strings.ToLower(m[1]))
				owned[strings.ToLower(m[2])] = true
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
