// SPDX-License-Identifier: Apache-2.0

import { expect, test, type Page, type Response } from '@playwright/test'

import { callbackError, classifyDexPage, dexPageError } from '../src/lib/drill-dex.ts'

// Lab gate of the restore drill (#983): a person signs in through the web UI's "Sign in with SSO", on the restored
// copy, through the login form of the drill's own Dex (a static user with an obviously fake password). The rest of
// the OIDC check is a Go HTTP flow in test/drill, on every profile; this is the one browser sign-in.
//
// The drill has no human, and Dex has no page that needs one, so the step does not wait for a heading. After each
// action it waits for the first of "back at Koinon" or a known Dex page (login form, wrong password, approval,
// connector chooser, second factor, error page) or a refusal of Koinon's callback, and throws a named error
// (web/src/lib/drill-dex.ts). Run by test/drill, not by hand:
//   pnpm test:e2e-drill oidc.spec
// Inputs (all set by the drill): KOINON_DRILL_WEB_URL, KOINON_DRILL_IDP_HOST and KOINON_DRILL_IDP_ADDR (the
// playwright.drill.config.ts maps the first to the second), KOINON_DRILL_OIDC_USER (the login: Dex asks for the
// e-mail address), KOINON_DRILL_OIDC_PASSWORD (the fake one), KOINON_DRILL_OIDC_EXPECT_USER_ID and
// KOINON_DRILL_OIDC_EXPECT_ROLE (the user ID and the default-org role the source recorded).

const need = (name: string): string => {
  const v = process.env[name]
  if (!v) throw new Error(`${name} is not set (the drill sets it)`)
  return v
}

const inboxHeading = (page: Page) => page.getByRole('heading', { name: 'Inbox', level: 1 })

// signInThroughDex clicks "Sign in with SSO" and drives the Dex form until the browser is back at Koinon. It
// polls the page every 250 ms, so each known state shows up within a poll and an unknown one at the deadline.
async function signInThroughDex(page: Page, login: string, password: string): Promise<void> {
  const web = new URL(need('KOINON_DRILL_WEB_URL')).origin
  const idp = need('KOINON_DRILL_IDP_HOST')

  // The callback's own answer: a 4xx or 5xx there is the install's refusal (state, nonce, token, disabled user,
  // restore release pending), not a page of Dex.
  let callback: Response | undefined
  page.on('response', (response) => {
    const url = new URL(response.url())
    if (url.origin === web && url.pathname === '/auth/oidc/callback') callback = response
  })

  await page.goto('/')
  await page.getByRole('link', { name: 'Sign in with SSO' }).click()

  const deadline = Date.now() + 60_000
  let submitted = false
  let last = 'the sign-in page'
  while (Date.now() < deadline) {
    if (callback) {
      const err = callbackError(callback.status(), callback.status() >= 400 ? await callback.text() : '')
      if (err) throw err
    }
    const url = new URL(page.url())
    if (url.origin === web && (await inboxHeading(page).isVisible().catch(() => false))) return
    if (url.host === idp) {
      // A navigation can destroy the page between the check and the read: try again at the next poll.
      const html = await page.content().catch(() => undefined)
      if (html !== undefined) {
        const dex = classifyDexPage(html)
        last = `dex page ${dex.kind}`
        const err = dexPageError(dex, login)
        if (err) throw err
        if (!submitted) {
          await page.locator('#login').fill(login)
          await page.locator('#password').fill(password)
          submitted = true
          await page.locator('#submit-login').click()
        }
      }
    } else if (url.origin === web) {
      last = `${url.pathname} on Koinon`
    }
    await page.waitForTimeout(250)
  }
  throw new Error(`timed out after 60 s waiting for Koinon or a known Dex page; last seen: ${last}`)
}

test('the admin signs in through the Dex form on the restored copy', async ({ page }) => {
  await signInThroughDex(page, need('KOINON_DRILL_OIDC_USER'), need('KOINON_DRILL_OIDC_PASSWORD'))
  await expect(inboxHeading(page)).toBeVisible()
  await page.reload() // the session holds
  await expect(inboxHeading(page)).toBeVisible()

  // Who the session is: the restored user, with the role the source recorded.
  const res = await page.request.post('/koinon.auth.v1alpha1.AuthService/WhoAmI', {
    headers: { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' },
    data: {},
  })
  expect(res.ok(), 'WhoAmI with the OIDC session').toBeTruthy()
  const me = await res.json()
  expect(me.user?.id, 'the same user as on the source').toBe(need('KOINON_DRILL_OIDC_EXPECT_USER_ID'))
  const roles = Object.fromEntries(
    (me.memberships ?? []).map((m: { orgSlug: string; role: string }) => [m.orgSlug, m.role]),
  )
  expect(String(roles.default).replace(/^ORG_ROLE_/, '').toLowerCase(), 'the role in the default org').toBe(
    need('KOINON_DRILL_OIDC_EXPECT_ROLE'),
  )
})
