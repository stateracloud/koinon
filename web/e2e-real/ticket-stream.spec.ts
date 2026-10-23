// SPDX-License-Identifier: Apache-2.0

import { expect, test } from '@playwright/test'

// W1 acceptance (issue #50): create a ticket and watch the run in the browser, against the real
// koinon-api, reconciler, agentd, Postgres, OpenShell (runc on kind) and the fake model server.
// The only doubles are the ones in test/e2e/kind/fakes.yaml: the fake model (script
// test/fakemodel/scripts/repo-helper.yaml) and the fake GitHub App API.

const fakeGitHub = process.env.KOINON_E2E_FAKEGITHUB_URL
const bootstrapPassword = process.env.KOINON_E2E_PASSWORD
const newPassword = 'e2e-browser-' + Math.random().toString(36).slice(2, 12)
const repository = 'koinon-demo/hello'
const title = 'Add hello.md'

test('sign in, create an agent and a ticket, watch the reply stream, approve, run succeeds', async ({ page, request }) => {
  expect(fakeGitHub, 'KOINON_E2E_FAKEGITHUB_URL').toBeTruthy()
  expect(bootstrapPassword, 'KOINON_E2E_PASSWORD').toBeTruthy()

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
  for (const col of ['To do', 'In progress', 'Waiting on you', 'Done']) {
    await expect(page.getByRole('heading', { level: 2, name: new RegExp(`^${col}`) })).toBeVisible()
  }

  // 2. GitHub connection to the fake GitHub App.
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

  // 3. Agent from the repo-helper template, bound to the repository, activated.
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Agents' }).click()
  await page.getByRole('link', { name: 'New agent' }).click()
  await page.getByRole('radio', { name: /Repo helper/ }).check()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('radio', { name: /OpenCode/ }).check()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('combobox', { name: 'Repository 1' }).fill('hello')
  await page.getByRole('option', { name: new RegExp(repository) }).click()
  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByRole('button', { name: 'Create draft' }).click()
  await expect(page.getByRole('heading', { name: 'Repo helper' })).toBeVisible()
  await page.getByRole('button', { name: 'Activate' }).click()
  await expect(page.getByText('Activated.')).toBeVisible()

  // 4. Ticket: the run starts in a sandbox and streams the agent's reply.
  await page.getByRole('link', { name: 'Create a ticket for it on the board' }).click()
  await page.getByLabel('Title').fill(title)
  await page.getByLabel('First message (optional)').fill(`Please add a hello.md to ${repository} that says hello from koinon.`)
  await page.getByRole('button', { name: 'Create ticket' }).click()
  await expect(page).toHaveURL(/\/tickets\/[^/]+$/)
  await expect(page.getByRole('heading', { name: title })).toBeVisible()

  const timeline = page.getByRole('list', { name: 'Timeline' })
  const first = timeline.getByText(/I asked for approval to open the pull request/).first()
  await expect(first).toBeVisible({ timeout: 8 * 60_000 })

  // 5. The tool call parked the run on an approval; approve it in the browser.
  const card = page.getByTestId('approval-card')
  await expect(card).toBeVisible()
  await expect(card.getByRole('region', { name: 'Files' })).toContainText('hello.md')
  await card.getByRole('button', { name: 'Approve' }).click()

  // 6. The run resumes and posts the pull request URL; it ends in a terminal state.
  await expect(timeline.getByText(/The pull request is open: https:\/\/github\.test\/koinon-demo\/hello\/pull\/1/)).toBeVisible({
    timeout: 5 * 60_000,
  })
  await expect(page.getByRole('region', { name: 'Run costs' })).toContainText('succeeded')
})
