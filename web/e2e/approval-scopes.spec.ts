// SPDX-License-Identifier: Apache-2.0

import { mkdirSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockApi } from './mock-api.ts'

const org = 'org-1'
const sha = 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
// Screenshots for the pull request; not committed.
const shots = process.env.INBOX_SCREENSHOTS

test('approve a pull request for this agent until a date, see it on the agent page, and revoke it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.emulateMedia({ colorScheme: 'light' })
  if (shots) mkdirSync(shots, { recursive: true })

  const approves: Record<string, unknown>[] = []
  const revokes: Record<string, unknown>[] = []
  let status = 'STATUS_PENDING'
  let scopes: Record<string, unknown>[] = []
  const until = new Date(Date.now() + 14 * 24 * 3600 * 1000)

  const approval = () => ({
    id: 'ap-1',
    orgId: org,
    ticketId: 't-1',
    runId: 'r-1',
    agentId: 'a-1',
    connector: 'github',
    tool: 'create_pull_request',
    target: 'github:acme/widgets',
    argsSha256: sha,
    decision: 'DECISION_ASK',
    status,
    canDecide: status === 'STATUS_PENDING',
    risk: { files: '1', bytes: '40' },
    pullRequest: { branch: 'koinon/lcm-week-1', title: 'Weekly lifecycle update', body: 'Updates the weekly report.', files: [{ path: 'docs/weekly.md', content: '# Weekly\n' }] },
    ...(status === 'STATUS_EXECUTED' ? { result: { url: 'https://github.com/acme/widgets/pull/7' } } : {}),
  })

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.approval.v1alpha1.ApprovalService/GetApproval': () => ({ approval: approval() }),
      'koinon.approval.v1alpha1.ApprovalService/GetApprovalDiff': () => ({
        base: 'main',
        files: [{ path: 'docs/weekly.md', status: 'FILE_STATUS_ADDED', unifiedDiff: '--- /dev/null\n+++ b/docs/weekly.md\n@@ -0,0 +1 @@\n+# Weekly\n' }],
      }),
      'koinon.approval.v1alpha1.ApprovalService/Approve': (req) => {
        approves.push(req)
        status = 'STATUS_EXECUTED'
        if (req.scope === 'SCOPE_KIND_AGENT') {
          scopes = [
            {
              id: 'sc-1',
              orgId: org,
              agentId: 'a-1',
              connector: 'github',
              tool: 'create_pull_request',
              target: 'github:acme/widgets',
              kind: 'SCOPE_KIND_AGENT',
              expiresAt: req.scopeUntil,
            },
          ]
        }
        return { approval: approval() }
      },
      'koinon.approval.v1alpha1.ApprovalService/ListApprovalScopes': () => ({ scopes }),
      'koinon.approval.v1alpha1.ApprovalService/RevokeApprovalScope': (req) => {
        revokes.push(req)
        scopes = []
        return { scope: { id: 'sc-1' } }
      },
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({
        agent: { id: 'a-1', orgId: org, displayName: 'LCM agent', status: 'AGENT_STATUS_ACTIVE', harness: 'opencode', latestVersion: 1, activeVersion: 1 },
      }),
    },
    stream: {},
  })

  await page.goto('/approvals/ap-1')
  const card = page.getByTestId('approval-card')
  await expect(card).toBeVisible()
  const choice = card.getByLabel('How far does this approval reach?')
  await expect(choice).toHaveValue('once')
  await choice.selectOption('agent')
  const date = card.getByLabel('Until (end of day)')
  await date.fill(until.toISOString().slice(0, 10))
  await expect(card.getByText(/always ask again/)).toBeVisible()
  await card.getByTestId('scope-choice').scrollIntoViewIfNeeded()
  if (shots) await page.screenshot({ path: `${shots}/approval-scope-choice.png`, fullPage: true })

  await card.getByRole('button', { name: 'Approve' }).click()
  await expect(card.getByRole('status')).toContainText('Done')
  expect(approves).toHaveLength(1)
  expect(approves[0]).toMatchObject({ approvalId: 'ap-1', argsSha256: sha, scope: 'SCOPE_KIND_AGENT' })
  expect(typeof (approves[0].scopeUntil as string)).toBe('string')

  await page.goto('/agents/a-1')
  const section = page.getByRole('region', { name: 'Scoped approvals' })
  const row = section.getByTestId('approval-scope')
  await expect(row).toContainText('create_pull_request on acme/widgets')
  await expect(row).toContainText('For this agent, until')
  if (shots) await page.screenshot({ path: `${shots}/agent-standing-approvals.png`, fullPage: true })

  await row.getByRole('button', { name: /Revoke/ }).click()
  await expect(section.getByText(/Every call that needs approval asks first/)).toBeVisible()
  expect(revokes).toEqual([{ orgId: org, scopeId: 'sc-1' }])
})
