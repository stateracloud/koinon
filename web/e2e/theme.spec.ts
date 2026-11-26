// SPDX-License-Identifier: Apache-2.0

import { mkdirSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockApi } from './mock-api.ts'

// Screenshots for the pull request; not committed.
const shots = process.env.THEME_SCREENSHOTS

test('theme: no wrong-theme flash, no font from a network host, K monogram, switch remembers', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 })
  if (shots) mkdirSync(shots, { recursive: true })
  const external: string[] = []
  const fonts: string[] = []
  page.on('request', (r) => {
    const u = new URL(r.url())
    if (u.hostname !== 'localhost') external.push(r.url())
    if (u.pathname.endsWith('.woff2')) fonts.push(r.url())
  })
  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: 'org-1', orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [] }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents: [] }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
    },
    stream: {},
  })

  // No saved choice: System, which follows the browser's preference (dark here), set before the app code runs.
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.goto('/inbox')
  await expect(page.getByRole('heading', { level: 1, name: 'Inbox' })).toBeVisible()
  const html = page.locator('html')
  await expect(html).toHaveAttribute('data-theme', 'dark')
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(28, 27, 41)')
  await expect(page.locator('body')).toHaveCSS('font-family', /Geist Variable/)
  await page.evaluate(`document.fonts.load('16px "Geist Variable"')`)
  expect(fonts.length, 'a Geist file loads from this origin').toBeGreaterThan(0)
  expect(await page.evaluate(`document.fonts.check('16px "Geist Variable"')`)).toBe(true)
  await expect(page.getByRole('link', { name: 'Koinon' }).locator('svg')).toBeVisible()
  await expect(page.locator('link[rel="icon"]')).toHaveAttribute('href', '/favicon.svg')
  if (shots) await page.screenshot({ path: `${shots}/dark.png` })

  // Light through the switch; the choice survives a reload with no flash (the attribute is set at once).
  await page.getByRole('button', { name: /^User menu:/ }).click()
  await page.getByRole('menuitemradio', { name: 'Light' }).click()
  await expect(html).toHaveAttribute('data-theme', 'light')
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(221, 221, 243)')
  if (shots) await page.screenshot({ path: `${shots}/light.png` })
  await page.reload()
  expect(await html.getAttribute('data-theme')).toBe('light')

  // System follows the preference.
  await page.getByRole('button', { name: /^User menu:/ }).click()
  await page.getByRole('menuitemradio', { name: 'System' }).click()
  await page.emulateMedia({ colorScheme: 'light' })
  await expect(html).toHaveAttribute('data-theme', 'light')
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect(html).toHaveAttribute('data-theme', 'dark')

  expect(external, 'no request leaves localhost (no font CDN)').toEqual([])
})
