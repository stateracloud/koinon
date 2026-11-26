// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'

import { expect, test } from '@playwright/test'

import { signIn } from './session.ts'

// Issue #1280 on a real install: an admin connects the fake MCP server (test/e2e/kind/fakes.yaml) in the
// console, sees its new tools wait for review, reviews one, and switches the server's outward calls off
// from the list with a reason. The server side of the same path is in test/e2e/mcp_test.go.

// The address koinon-api and the gateway reach the fake by, and its fake key (test/e2e/kind/fakes.yaml).
const fakeMCPURL = 'https://fakemcp.koinon-e2e-fakes.svc:8080/mcp'
const fakeMCPKey = 'not-a-real-token'
// The CA the fake made at start, a public certificate that test/e2e/run.sh saved.
const fakeMCPCA = process.env.KOINON_E2E_FAKEMCP_CA

test('connect the fake MCP server, review a new tool, switch its outward calls off with a reason', async ({ page }) => {
  expect(fakeMCPCA, 'KOINON_E2E_FAKEMCP_CA').toBeTruthy()
  await signIn(page)

  // 1. Connect the server. The key is write-only: it is not on the page afterwards.
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'MCP servers' }).click()
  await page.getByRole('link', { name: 'Connect a remote server' }).click()
  const form = page.getByRole('form', { name: 'Connect a remote server' })
  await form.getByLabel('Name', { exact: true }).fill('tracker-web')
  await form.getByLabel('URL').fill(fakeMCPURL)
  await form.getByLabel('Credential').selectOption('custom')
  await form.getByLabel('Header name').fill('X-Api-Key')
  await form.getByLabel('Header value').fill(fakeMCPKey)
  await form.getByLabel('CA certificate (optional)').fill(readFileSync(fakeMCPCA!, 'utf8'))
  await form.getByRole('button', { name: 'Connect server' }).click()
  await expect(page.getByRole('heading', { name: 'tracker-web', level: 1 })).toBeVisible()
  await expect(page.getByText(fakeMCPKey)).toHaveCount(0)

  // 2. The prober lists the tools through the gateway. A new credential reaches the gateway through the
  // kubelet's Secret sync first, so this can take minutes (the Go e2e waits up to 8).
  const tools = page.getByRole('list', { name: 'Tools of tracker-web' })
  await expect(async () => {
    await page.reload()
    await expect(tools.getByRole('button', { name: 'Mark reviewed: write_item' })).toBeVisible({ timeout: 5_000 })
  }).toPass({ timeout: 8 * 60_000, intervals: [5_000] })

  // 3. A new tool waits for review: agents do not get it until an admin approves its text (ADR 0125).
  const writeItem = tools.getByRole('listitem').filter({ has: page.getByRole('button', { name: 'Mark reviewed: write_item' }) })
  await expect(writeItem.getByText('Waits for review')).toBeVisible()
  await expect(page.getByRole('note', { name: 'Tools waiting for review: tracker-web' })).toBeVisible()
  await writeItem.getByRole('button', { name: 'Mark reviewed: write_item' }).click()
  await expect(tools.getByRole('button', { name: 'Mark reviewed: write_item' })).toHaveCount(0)
  // The other new tools still wait.
  await expect(tools.getByRole('button', { name: 'Mark reviewed: read_item' })).toBeVisible()

  // 4. The outward switch on the list row records the reason (ADR 0110).
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'MCP servers' }).click()
  const row = page.getByRole('table', { name: 'MCP servers' }).getByRole('row').filter({ has: page.getByRole('link', { name: 'tracker-web', exact: true }) })
  await row.getByLabel('Reason for switching off server tracker-web').fill('browser e2e check')
  await row.getByRole('button', { name: 'Switch off outward calls of server tracker-web' }).click()
  await expect(row.getByRole('note')).toContainText('browser e2e check')
  // Enable it again, so the install is as the next spec expects.
  await row.getByRole('button', { name: 'Enable outward calls of server tracker-web' }).click()
  await expect(row.getByRole('button', { name: 'Switch off outward calls of server tracker-web' })).toBeVisible()
})
