// SPDX-License-Identifier: Apache-2.0

import { randomBytes } from 'node:crypto'

import { expect, test } from '@playwright/test'

// W1 acceptance (old issue 50): create a task and watch the run in the browser, against the real
// koinon-api, reconciler, agentd, Postgres, OpenShell (runc on kind) and the fake model server.
// The only doubles are the ones in test/e2e/kind/fakes.yaml: the fake model (script
// test/fakemodel/scripts/repo-helper.yaml) and the fake GitHub App API.

const fakeGitHub = process.env.KOINON_E2E_FAKEGITHUB_URL
const bootstrapPassword = process.env.KOINON_E2E_PASSWORD
const newPassword = 'e2e-browser-' + randomBytes(8).toString('hex')
// The fake model server in the cluster (test/e2e/run.sh passes the address the gateway reaches it by).
const fakeModelURL = process.env.KOINON_E2E_FAKEMODEL_URL
const repository = 'koinon-demo/hello'
const title = 'Add hello.md'

test('sign in, create an agent and a task, watch the reply stream, approve, run succeeds', async ({ page, request }) => {
  expect(fakeGitHub, 'KOINON_E2E_FAKEGITHUB_URL').toBeTruthy()
  expect(bootstrapPassword, 'KOINON_E2E_PASSWORD').toBeTruthy()
  expect(fakeModelURL, 'KOINON_E2E_FAKEMODEL_URL').toBeTruthy()

  // 1. Bootstrap admin: sign in, replace the one-time password.
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Sign in to Koinon' })).toBeVisible()
  await page.getByLabel('Password').fill(bootstrapPassword!)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('heading', { name: 'Choose a new password' })).toBeVisible()
  await page.getByLabel('Current password').fill(bootstrapPassword!)
  await page.getByLabel('New password', { exact: true }).fill(newPassword)
  await page.getByLabel('Repeat new password').fill(newPassword)
  await page.getByRole('button', { name: 'Change password' }).click()
  // The home screen is the inbox; the board is its secondary view, at /board.
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible()
  await expect(page.getByRole('heading', { level: 2, name: 'Waiting on you' })).toBeVisible()

  // 2. Model provider: added in the UI (the fake model server as an OpenAI-compatible provider), with
  // one model. The key is write-only: it is not on the page afterwards.
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Providers' }).click()
  await expect(page.getByRole('heading', { name: 'Providers', level: 1 })).toBeVisible()
  await page.getByRole('button', { name: 'Add provider' }).click()
  const providerForm = page.getByRole('form', { name: 'Add provider' })
  await providerForm.getByLabel('Name', { exact: true }).fill('fake')
  await providerForm.getByLabel('Provider type').selectOption('openai')
  await providerForm.getByLabel('URL').fill(fakeModelURL!)
  await providerForm.getByLabel('API key').fill('e2e-dummy-key')
  await providerForm.getByRole('button', { name: 'Add provider' }).click()
  const provider = page.getByRole('region', { name: 'fake' })
  await expect(provider).toBeVisible()
  await expect(provider).toContainText('Stored encrypted; it cannot be shown')
  await expect(page.locator('body')).not.toContainText('e2e-dummy-key')
  await provider.getByRole('button', { name: 'Add model to fake' }).click()
  const modelForm = page.getByRole('form', { name: 'Add model to fake' })
  await modelForm.getByLabel('Model name').fill('repo-helper')
  await modelForm.getByRole('button', { name: 'Add model', exact: true }).click()
  await expect(provider.getByRole('list', { name: 'Models of fake' })).toContainText('default/repo-helper')

  // 2b. The model's price is an org price (the fake model is in no default list); an org cap is set and shown.
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Prices' }).click()
  await expect(page.getByRole('heading', { name: 'Prices', level: 1 })).toBeVisible()
  await page.getByRole('button', { name: 'Add price' }).first().click()
  const priceForm = page.getByRole('form', { name: 'Add a price' })
  await priceForm.getByLabel('Name', { exact: true }).fill('fake-repo-helper')
  await priceForm.getByLabel('Provider type').fill('openai')
  await priceForm.getByLabel('Model pattern').fill('repo-helper')
  await priceForm.getByLabel('Input', { exact: true }).fill('0.30')
  await priceForm.getByLabel('Output', { exact: true }).fill('1.20')
  await priceForm.getByLabel('Largest output (tokens)').fill('4096')
  await priceForm.getByRole('button', { name: 'Add price' }).click()
  await expect(page.getByRole('list', { name: 'Your prices' })).toContainText('fake-repo-helper')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Budgets' }).click()
  const cap = page.getByRole('region', { name: 'Monthly cap' })
  await cap.getByRole('button', { name: 'Set a cap' }).click()
  await cap.getByLabel('Monthly cap in US dollars').fill('50')
  await cap.getByRole('button', { name: 'Save cap' }).click()
  await expect(cap).toContainText('$50.00')
  await expect(cap).toContainText('Spent this month')

  // A UI-added provider is not served at once: the reconciler picks the row up on its 30 s resync, then
  // the key reaches agentgateway through the kubelet's Secret sync. That took about 170 s on kind. A run
  // that starts first fails with "provider authentication required". Nothing in the UI shows when the
  // gateway has it yet, so wait it out before the agent's first run.
  await page.waitForTimeout(240_000)

  // 3. GitHub connection to the fake GitHub App.
  const app = (await (await request.get(`${fakeGitHub}/_fake/app`)).json()) as {
    appId: number
    installationId: number
    privateKey: string
  }
  await page.goto('/connections')
  await page.getByRole('link', { name: 'Connect GitHub' }).click()
  await page.getByLabel('Name', { exact: true }).fill('Fake GitHub')
  await page.getByLabel('App ID').fill(String(app.appId))
  await page.getByLabel('Installation ID').fill(String(app.installationId))
  await page.getByLabel('Private key', { exact: true }).fill(app.privateKey)
  await page.getByRole('button', { name: 'Create and test connection' }).click()
  await expect(page.getByRole('heading', { name: 'Fake GitHub' })).toBeVisible()
  await expect(page.getByRole('list', { name: 'Installation repositories' })).toContainText(repository)

  // 4. Agent from the repo-helper template, bound to the repository, activated.
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Agents' }).click()
  await page.getByRole('link', { name: 'New agent' }).click()
  // Anchored: "Repo helper (Gitea)" is a template too (old issue 370).
  await page.getByRole('radio', { name: /^Repo helper v/ }).check()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('radio', { name: /OpenCode/ }).check()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('combobox', { name: 'Repository 1' }).fill('hello')
  await page.getByRole('option', { name: new RegExp(repository) }).click()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('button', { name: 'Create draft' }).click()
  await expect(page.getByRole('heading', { name: 'Repo helper', exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Activate' }).click()
  await expect(page.getByText('Activated.')).toBeVisible()

  // 5. Task: the run starts in a sandbox and streams the agent's reply.
  await page.getByRole('link', { name: 'Hand it a task from the inbox' }).click()
  await page.getByLabel('Title').fill(title)
  await page.getByLabel('First message (optional)').fill(`Please add a hello.md to ${repository} that says hello from koinon.`)
  await page.getByRole('button', { name: 'Create task' }).click()
  // The new task opens in place as a conversation next to the inbox; open its page.
  await expect(page.getByRole('region', { name: `Conversation: ${title}` })).toBeVisible()
  await page.getByRole('link', { name: 'Open task' }).click()
  await expect(page).toHaveURL(/\/tasks\/[^/]+$/)
  await expect(page.getByRole('heading', { name: title })).toBeVisible()

  const timeline = page.getByRole('list', { name: 'Timeline' })
  // The reply shows exactly once: streamed live, then replaced by the persisted message.
  await expect(timeline.getByText(/I asked for approval to open the pull request/)).toHaveCount(1, { timeout: 8 * 60_000 })

  // 6. The tool call parked the run on an approval; approve it in the browser.
  const card = page.getByTestId('approval-card')
  await expect(card).toBeVisible()
  await expect(card.getByRole('region', { name: 'Files' })).toContainText('hello.md')
  await card.getByRole('button', { name: 'Approve' }).click()

  // 7. The run resumes and posts the pull request URL; it ends in a terminal state.
  await expect(timeline.getByText(/The pull request is open: https:\/\/github\.test\/koinon-demo\/hello\/pull\/1/)).toHaveCount(1, {
    timeout: 5 * 60_000,
  })
  await expect(page.getByRole('region', { name: 'Run costs' })).toContainText('succeeded')
})
