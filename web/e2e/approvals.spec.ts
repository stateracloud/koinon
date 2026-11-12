// SPDX-License-Identifier: Apache-2.0

import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
const now = () => new Date().toISOString()
const sha = 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
// Only the shape of a PEM block: not a key.
const pem = '-----BEGIN RSA PRIVATE KEY-----\nE2E-ONLY-NOT-A-KEY\n-----END RSA PRIVATE KEY-----'

const connectorType = {
  id: 'github',
  version: '0.2.0',
  displayName: 'GitHub',
  description: 'Read repositories and propose pull requests.',
  authType: 'app-credential',
  document: {
    spec: {
      targets: { kind: 'repository', pattern: '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$' },
      setupGuide: {
        fields: [
          { id: 'appId', label: 'App ID', type: 'integer' },
          { id: 'installationId', label: 'Installation ID', type: 'integer' },
          { id: 'privateKey', label: 'Private key', type: 'pem', secret: true },
        ],
        steps: [
          { id: 'create-app', title: 'Create a GitHub App', body: 'Turn **Webhook** off.', fields: ['appId'] },
          { id: 'generate-key', title: 'Generate a private key', body: 'Paste the `.pem` file.', fields: ['privateKey'], check: 'github.app' },
          { id: 'install-app', title: 'Install the App on your repositories', body: 'Pick the repositories.', fields: ['installationId'], check: 'github.installation' },
          { id: 'test-token', title: 'Test the connection', body: 'Nothing is written to GitHub.', check: 'github.token' },
        ],
      },
    },
  },
}

const template = {
  id: 'tpl-1',
  slug: 'repo-helper',
  version: '0.1.0',
  displayName: 'Repo helper',
  description: 'Makes small changes to one repository.',
  allowedHarnesses: ['opencode'],
  defaultHarness: 'opencode',
  model: 'default',
  grants: [{ connector: 'github', scopes: ['repo.read', 'pull_request.create', 'issue.comment'], preset: 'PRESET_STANDARD', targetsMin: 1, targetsMax: 1 }],
  runLimits: { maxDurationSeconds: '1800', maxModelRequests: '300', maxSpendUsd: '5.00' },
}

test('connect GitHub, create and activate an agent, then approve its pull request', async ({ page }) => {
  const connections: Record<string, unknown>[] = []
  const created: Record<string, unknown>[] = []
  const approved: Record<string, unknown>[] = []
  let agent: Record<string, unknown> | undefined
  const events: Record<string, unknown>[] = []
  let taskState = 'TASK_STATE_SUBMITTED'
  let approvalStatus = 'STATUS_PENDING'
  let runState = 'RUN_STATE_PARKED'

  const addEvent = (kind: string, payload: Record<string, unknown>, actorKind = 'ACTOR_KIND_SYSTEM') =>
    events.push({ seq: String(events.length + 1), kind, actorKind, payload, createdAt: now(), runId: kind === 'message' ? '' : 'r-1' })
  const base = { approval_id: 'ap-1', tool: 'create_pull_request', target: 'github:acme/widgets', args_sha256: sha }
  const task = () => ({ id: 't-1', orgId: org, title: 'Fix the README typo', state: taskState, agentId: 'a-1' })
  const run = () => ({ id: 'r-1', orgId: org, ticketId: 't-1', agentId: 'a-1', state: runState })
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
    status: approvalStatus,
    canDecide: approvalStatus === 'STATUS_PENDING',
    risk: { files: '2', bytes: '64', dependencies: ['package.json'] },
    pullRequest: {
      branch: 'koinon/fix-readme-typo',
      title: 'Fix a typo in the README',
      body: 'Fixes "recieve".',
      files: [
        { path: 'README.md', content: '# Widgets\nreceive\n' },
        { path: 'package.json', content: '{"name":"widgets"}\n' },
      ],
    },
    ...(approvalStatus === 'STATUS_EXECUTED' ? { result: { url: 'https://github.com/acme/widgets/pull/7', number: 7 } } : {}),
  })

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnectorTypes': () => ({ connectorTypes: [connectorType] }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections }),
      'koinon.connection.v1alpha1.ConnectionService/CreateGitHubConnection': (req) => {
        created.push(req)
        const lastTest = [
          { check: 'github.app', step: 'generate-key', ok: true, message: 'App koinon-e2e' },
          { check: 'github.installation', step: 'install-app', ok: true, message: '1 repository' },
          { check: 'github.token', step: 'test-token', ok: true, message: 'token minted' },
        ]
        connections.push({
          id: 'c-1',
          orgId: org,
          connector: 'github',
          displayName: req.displayName,
          status: 'CONNECTION_STATUS_OK',
          lastTest,
          github: { appId: String(req.appId), installationId: String(req.installationId), keyFingerprint: 'SHA256:e2e' },
        })
        return { connection: connections[0], results: lastTest }
      },
      'koinon.connection.v1alpha1.ConnectionService/GetConnection': () => ({ connection: connections[0] }),
      'koinon.connection.v1alpha1.ConnectionService/ListInstallationRepos': (req) => {
        const repos = [
          { fullName: 'acme/widgets', private: true, defaultBranch: 'main' },
          { fullName: 'acme/gadgets', defaultBranch: 'main' },
        ]
        const q = String(req.query ?? '').toLowerCase()
        return { repos: repos.filter((r) => r.fullName.includes(q)), totalCount: String(repos.length) }
      },
      'koinon.template.v1alpha1.TemplateService/ListTemplates': () => ({ templates: [template] }),
      'koinon.agent.v1alpha1.AgentService/CreateAgentFromTemplate': (req) => {
        expect(req).toMatchObject({ orgId: org, templateId: 'tpl-1', grants: [{ connector: 'github', connectionId: 'c-1', repositories: ['acme/widgets'] }] })
        agent = {
          id: 'a-1',
          orgId: org,
          displayName: req.displayName,
          status: 'AGENT_STATUS_DRAFT',
          harness: 'opencode',
          templateSlug: 'repo-helper',
          latestVersion: 1,
          proposedGrants: [{ connector: 'github', scopes: template.grants[0].scopes, preset: 'PRESET_STANDARD', connectionId: 'c-1', repositories: ['acme/widgets'] }],
        }
        return { agent }
      },
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({ agent }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents: agent ? [agent] : [] }),
      'koinon.agent.v1alpha1.AgentService/ActivateAgent': (req) => {
        expect(req).toMatchObject({ agentId: 'a-1', version: 1 })
        agent = { ...agent, status: 'AGENT_STATUS_ACTIVE', activeVersion: 1, approvedGrants: agent?.proposedGrants }
        return { agent }
      },
      'koinon.task.v1alpha1.TaskService/CreateTask': (req) => {
        expect(req).toMatchObject({ agentId: 'a-1', title: 'Fix the README typo' })
        addEvent('task.created', { title: req.title })
        addEvent('run.queued', {})
        // The agent asks for a pull request and its run parks (P1).
        addEvent('approval.requested', { ...base, decision: 'ask' }, 'ACTOR_KIND_AGENT')
        addEvent('run.parked', { reason: 'approval', approvals: ['ap-1'] })
        taskState = 'TASK_STATE_INPUT_REQUIRED'
        return { task: task(), run: run() }
      },
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: events.length ? [task()] : [] }),
      'koinon.task.v1alpha1.TaskService/GetTask': () => ({ task: task(), events, runs: [run()] }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: approvalStatus === 'STATUS_PENDING' && events.length ? [approval()] : [] }),
      'koinon.approval.v1alpha1.ApprovalService/GetApproval': () => ({ approval: approval() }),
      'koinon.approval.v1alpha1.ApprovalService/GetApprovalDiff': () => ({
        base: 'main',
        files: [
          {
            path: 'README.md',
            status: 'FILE_STATUS_MODIFIED',
            unifiedDiff: '--- a/README.md\n+++ b/README.md\n@@ -1,2 +1,2 @@\n # Widgets\n-recieve\n+receive\n',
          },
          { path: 'package.json', fallbackReason: 'GitHub answered 502 Bad Gateway when Koinon read the current version' },
        ],
      }),
      'koinon.approval.v1alpha1.ApprovalService/Approve': (req) => {
        approved.push(req)
        if (req.argsSha256 !== sha) return { error: { code: 'failed_precondition', message: 'arguments changed' } }
        approvalStatus = 'STATUS_EXECUTED'
        addEvent('approval.approved', { ...base, note: req.note }, 'ACTOR_KIND_USER')
        addEvent('approval.executed', { ...base, result: { url: 'https://github.com/acme/widgets/pull/7' } })
        taskState = 'TASK_STATE_WORKING'
        runState = 'RUN_STATE_SUCCEEDED'
        return { approval: approval() }
      },
    },
    stream: {
      'koinon.run.v1alpha1.RunService/WatchRun': (): Stream => ({ messages: [{ run: run() }] }),
    },
  })

  // 1. Connect GitHub through the setup guide.
  await page.goto('/connections')
  await page.getByRole('link', { name: 'Connect GitHub' }).click()
  await expect(page.getByRole('heading', { name: '2. Generate a private key' })).toBeVisible()
  await page.getByLabel('Name', { exact: true }).fill('Acme GitHub')
  await page.getByLabel('App ID').fill('12')
  await page.getByLabel('Installation ID').fill('34')
  await page.getByLabel('Private key', { exact: true }).fill(pem)
  await page.getByRole('button', { name: 'Create and test connection' }).click()
  await expect(page.getByRole('heading', { name: 'Acme GitHub' })).toBeVisible()
  await expect(page.getByRole('list', { name: 'Checks for Test the connection' })).toContainText('token minted')
  expect(created[0]).toMatchObject({ appId: '12', installationId: '34', privateKey: pem, test: true })
  await expect(page.locator('body')).not.toContainText('E2E-ONLY-NOT-A-KEY')
  await expect(page.getByRole('list', { name: 'Installation repositories' })).toContainText('acme/gadgets')

  // 2. Create an agent from the template and activate it.
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Agents' }).click()
  await page.getByRole('link', { name: 'New agent' }).click()
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByRole('radio', { name: /OpenCode/ })).toBeChecked()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('combobox', { name: 'Repository 1' }).fill('widg')
  await page.getByRole('option', { name: /acme\/widgets/ }).click()
  await expect(page.getByRole('combobox', { name: 'Repository 1' })).toHaveValue('acme/widgets')
  await page.getByRole('button', { name: 'Next' }).click()
  await expect(page.getByRole('region', { name: 'Draft summary' })).toContainText('Connection: Acme GitHub')
  await page.getByRole('button', { name: 'Create draft' }).click()
  await expect(page.getByRole('heading', { name: 'Repo helper' })).toBeVisible()
  await page.getByRole('button', { name: 'Activate' }).click()
  await expect(page.getByText('Activated.')).toBeVisible()

  // 3. Hand it a task; it asks for approval.
  await page.getByRole('link', { name: 'Hand it a task from the inbox' }).click()
  await page.getByLabel('Title').fill('Fix the README typo')
  await page.getByRole('button', { name: 'Create task' }).click()
  await page.getByRole('link', { name: 'Open task' }).click()
  await expect(page).toHaveURL(/\/tasks\/t-1$/)
  const card = page.getByTestId('approval-card')
  await expect(card.getByRole('region', { name: 'Push and run risk' })).toContainText('Dependency manifests (1)')
  const files = card.getByRole('region', { name: 'Files' })
  await files.getByText('README.md').click()
  await expect(files.getByLabel('Diff of README.md')).toContainText('+receive')
  await files.getByText('package.json').click()
  await expect(files.getByRole('note')).toContainText('GitHub answered 502')

  // 4. The inbox lists it; approve from the standalone view.
  await page.getByRole('link', { name: '← Inbox' }).click()
  const waiting = page.getByRole('list', { name: 'Waiting on you' })
  await waiting.getByRole('link', { name: 'Review create_pull_request on acme/widgets' }).click()
  await expect(page.getByRole('heading', { name: 'Approval request' })).toBeVisible()
  await page.getByLabel('Note (optional)').fill('ship it')
  await page.getByRole('button', { name: 'Approve' }).click()
  await expect(page.getByRole('link', { name: 'https://github.com/acme/widgets/pull/7' })).toBeVisible()
  expect(approved).toEqual([{ orgId: org, approvalId: 'ap-1', argsSha256: sha, note: 'ship it' }])

  // 5. The result is on the task's timeline.
  await page.getByRole('link', { name: 'Open the task' }).click()
  const timeline = page.getByRole('list', { name: 'Timeline' })
  await expect(timeline.getByText('Approved create_pull_request on acme/widgets: ship it')).toBeVisible()
  await expect(timeline.getByText('Done: create_pull_request on acme/widgets')).toBeVisible()
})
