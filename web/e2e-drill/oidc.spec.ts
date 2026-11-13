// SPDX-License-Identifier: Apache-2.0

import { expect, test, type Page } from '@playwright/test'

// Lab gate of the restore drill (#927): a person can sign in through the OIDC issuer on the restored copy,
// with a passkey. The drill has no human, so the browser gets a virtual WebAuthn authenticator (Chrome's
// DevTools protocol) that holds the test user's credential. The credential comes from the environment the
// drill passes in (KOINON_DRILL_OIDC_*); nothing real is committed. Run by test/drill, not by hand:
//   pnpm test:e2e-drill oidc.spec
// The IdP's own page is not ours: the steps below click what a standing "koinon-drill" client shows (a
// username field when it asks for one, then the passkey button) and wait for the redirect back.
const username = process.env.KOINON_DRILL_OIDC_USER
const credentialId = process.env.KOINON_DRILL_OIDC_CREDENTIAL_ID // base64url
const privateKey = process.env.KOINON_DRILL_OIDC_PRIVATE_KEY // base64 PKCS#8
const userHandle = process.env.KOINON_DRILL_OIDC_USER_HANDLE // base64url
const rpId = process.env.KOINON_DRILL_OIDC_RP_ID // the IdP's host name

async function authenticator(page: Page, withCredential: boolean) {
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('WebAuthn.enable')
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  })
  if (withCredential) {
    await cdp.send('WebAuthn.addCredential', {
      authenticatorId,
      credential: {
        credentialId: credentialId!,
        isResidentCredential: true,
        rpId: rpId!,
        privateKey: privateKey!,
        userHandle: userHandle!,
        signCount: 0,
      },
    })
  }
}

async function startSSO(page: Page) {
  await page.goto('/')
  await page.getByRole('link', { name: 'Sign in with SSO' }).click()
  // On the issuer's page. Some ask for the user name first, some go straight to the passkey.
  const name = page.locator('input[name="username"], input#username').first()
  if (await name.isVisible({ timeout: 15_000 }).catch(() => false)) {
    await name.fill(username!)
    await page.keyboard.press('Enter')
  }
  await page.getByRole('button', { name: /passkey|security key|sign in|log in/i }).first().click({ timeout: 15_000 })
}

test.beforeEach(() => {
  expect(username, 'KOINON_DRILL_OIDC_USER').toBeTruthy()
  expect(credentialId, 'KOINON_DRILL_OIDC_CREDENTIAL_ID').toBeTruthy()
  expect(privateKey, 'KOINON_DRILL_OIDC_PRIVATE_KEY').toBeTruthy()
  expect(userHandle, 'KOINON_DRILL_OIDC_USER_HANDLE').toBeTruthy()
  expect(rpId, 'KOINON_DRILL_OIDC_RP_ID').toBeTruthy()
})

test('sign in through the OIDC issuer with the passkey', async ({ page }) => {
  await authenticator(page, true)
  await startSSO(page)
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible({ timeout: 60_000 })
  // The session holds after a reload.
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible()
})

test('an authenticator without the credential does not sign in', async ({ page }) => {
  await authenticator(page, false)
  await startSSO(page).catch(() => undefined) // the issuer may refuse before the button exists
  await page.waitForTimeout(10_000)
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toHaveCount(0)
  expect(new URL(page.url()).origin).not.toBe(new URL(process.env.KOINON_DRILL_WEB_URL!).origin)
})
