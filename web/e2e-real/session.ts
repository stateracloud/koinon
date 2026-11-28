// SPDX-License-Identifier: Apache-2.0

import { randomBytes } from 'node:crypto'

import { expect, type Page } from '@playwright/test'

// The specs share one install, and the first sign-in replaces the one-time bootstrap password. Every spec
// signs in with signIn, so the specs run in any order. test/e2e/run.sh sets KOINON_E2E_NEW_PASSWORD, so a
// restarted worker still knows the password; a local run without it uses one random password per worker.
const bootstrapPassword = process.env.KOINON_E2E_PASSWORD
const newPassword = process.env.KOINON_E2E_NEW_PASSWORD || 'e2e-browser-' + randomBytes(8).toString('hex')

/** signIn signs the bootstrap admin in. On the first sign-in of the install it also replaces the one-time password. */
export async function signIn(page: Page) {
  expect(bootstrapPassword, 'KOINON_E2E_PASSWORD').toBeTruthy()
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Sign in to Koinon' })).toBeVisible()
  await page.getByLabel('Password').fill(bootstrapPassword!)
  await page.getByRole('button', { name: 'Sign in' }).click()
  const choose = page.getByRole('heading', { name: 'Choose a new password' })
  const refused = page.getByRole('alert')
  await expect(choose.or(refused)).toBeVisible()
  if (await choose.isVisible()) {
    await page.getByLabel('Current password').fill(bootstrapPassword!)
    await page.getByLabel('New password', { exact: true }).fill(newPassword)
    await page.getByLabel('Repeat new password').fill(newPassword)
    await page.getByRole('button', { name: 'Change password' }).click()
  } else {
    // Another spec already replaced the bootstrap password.
    await page.getByLabel('Password').fill(newPassword)
    await page.getByRole('button', { name: 'Sign in' }).click()
  }
  await expect(page.getByRole('navigation', { name: 'Settings' })).toBeVisible()
}
