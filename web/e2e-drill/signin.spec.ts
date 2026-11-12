// SPDX-License-Identifier: Apache-2.0

import { expect, test } from '@playwright/test'

// Lab gate of the restore drill (ADR 0109, #518): a person can sign in through the browser on the restored
// copy. The API checks of the drill sign in by API; this proves the shipped web UI works against the restored
// install too. The password is the one of the restored admin (the drill seeds a fixed fake one; after a real
// disaster the operator gets one from the break-glass request, #512).
const username = process.env.KOINON_DRILL_WEB_USER ?? 'admin'
const password = process.env.KOINON_DRILL_WEB_PASSWORD

test('sign in through the browser on the restored copy', async ({ page }) => {
  expect(password, 'KOINON_DRILL_WEB_PASSWORD').toBeTruthy()

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Sign in to Koinon' })).toBeVisible()
  await page.getByLabel('Username').fill(username)
  await page.getByLabel('Password').fill(password!)
  await page.getByRole('button', { name: 'Sign in' }).click()

  // The restored admin has already chosen a password: no "Choose a new password" step, the inbox is home.
  await expect(page.getByRole('heading', { name: 'Choose a new password' })).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible()
  // The session holds after a reload: the cookie the restored API set is accepted by the restored API.
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Sign in to Koinon' })).toHaveCount(0)
})
