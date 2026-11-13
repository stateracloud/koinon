// SPDX-License-Identifier: Apache-2.0

import { readFileSync, writeFileSync } from 'node:fs'

import { expect, test, type CDPSession, type Page } from '@playwright/test'

// Lab gate of the restore drill (#927): a person can sign in through the OIDC issuer, on the source before it is
// lost and on the restored copy, as an admin and as a member, and the same user holds the same role on both. The
// drill has no human, so the browser gets a virtual WebAuthn authenticator (Chrome's DevTools protocol) that holds
// the persona's passkey. The passkeys come from the environment the drill passes in
// (KOINON_DRILL_OIDC_<ADMIN|MEMBER>_<FIELD>, test/drill OIDCEnvFields); nothing real is committed, logged or traced.
// Run by test/drill, not by hand:
//   pnpm test:e2e-drill oidc.spec
// The IdP's own page is not ours: the steps click what the standing public client shows (a username field when
// it asks for one, then the passkey button) and wait for the redirect back.
//
// Outputs (paths come from the drill):
//   KOINON_DRILL_OIDC_ROLES_OUT      per persona: username, user ID and the role in each org, from WhoAmI
//   KOINON_DRILL_OIDC_SIGNCOUNT_OUT  per persona: username and the authenticator's signCount after the sign-in,
//                                    read back with WebAuthn.getCredentials (never key material)
// KOINON_DRILL_OIDC_NEGATIVE=1 adds the negative case: an authenticator without the credential does not sign in.
const personas = ['admin', 'member'] as const
type Persona = (typeof personas)[number]

const need = (name: string): string => {
  const v = process.env[name]
  if (!v) throw new Error(`${name} is not set (see test/drill/topology/lab.example.yaml)`)
  return v
}
const env = (persona: Persona, field: string): string => need(`KOINON_DRILL_OIDC_${persona.toUpperCase()}_${field}`)

type Authenticator = { cdp: CDPSession; authenticatorId: string }

async function authenticator(page: Page, persona: Persona | null): Promise<Authenticator> {
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
  if (persona) {
    await cdp.send('WebAuthn.addCredential', {
      authenticatorId,
      credential: {
        credentialId: env(persona, 'CREDENTIAL_ID'), // base64url
        isResidentCredential: true,
        rpId: env(persona, 'RP_ID'),
        privateKey: env(persona, 'PRIVATE_KEY'), // PKCS#8, base64
        userHandle: env(persona, 'USER_HANDLE'), // base64
        signCount: Number(env(persona, 'SIGN_COUNT')),
      },
    })
  }
  return { cdp, authenticatorId }
}

async function startSSO(page: Page, persona: Persona) {
  const username = env(persona, 'USERNAME')
  await page.goto('/')
  await page.getByRole('link', { name: 'Sign in with SSO' }).click()
  // Koinon sent the browser to the issuer (the persona's ORIGIN), not anywhere else.
  await page.waitForURL((u) => u.origin === env(persona, 'ORIGIN'), { timeout: 30_000 })
  const name = page.locator('input[name="username"], input#username').first()
  if (await name.isVisible({ timeout: 15_000 }).catch(() => false)) {
    await name.fill(username)
    await page.keyboard.press('Enter')
  }
  await page.getByRole('button', { name: /passkey|security key|sign in|log in/i }).first().click({ timeout: 15_000 })
}

// merge adds one persona's entry to a JSON object file the drill names.
function merge(path: string, persona: Persona, entry: Record<string, unknown>) {
  let cur: Record<string, unknown> = {}
  try {
    cur = JSON.parse(readFileSync(path, 'utf8'))
  } catch {
    // the first persona creates the file
  }
  cur[persona] = entry
  writeFileSync(path, JSON.stringify(cur, null, 2) + '\n', { mode: 0o600 })
}

for (const persona of personas) {
  test(`${persona} signs in through the OIDC issuer with the passkey`, async ({ page }) => {
    const username = env(persona, 'USERNAME')
    const auth = await authenticator(page, persona)
    await startSSO(page, persona)
    await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible({ timeout: 60_000 })
    await page.reload() // the session holds
    await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toBeVisible()

    // Who the session is and which role it holds in each org: the drill compares the source's with the restored copy's.
    const res = await page.request.post('/koinon.auth.v1alpha1.AuthService/WhoAmI', {
      headers: { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' },
      data: {},
    })
    expect(res.ok(), 'WhoAmI with the OIDC session').toBeTruthy()
    const me = await res.json()
    const roles = Object.fromEntries(
      (me.memberships ?? []).map((m: { orgSlug: string; role: string }) => [m.orgSlug, m.role]),
    )
    merge(need('KOINON_DRILL_OIDC_ROLES_OUT'), persona, { username, userId: me.user?.id, roles })

    // The authenticator counted the sign-in. The lab writes the new count back, so the next run does not replay.
    const { credentials } = await auth.cdp.send('WebAuthn.getCredentials', { authenticatorId: auth.authenticatorId })
    const mine = credentials.find((c) => c.credentialId === env(persona, 'CREDENTIAL_ID'))
    expect(mine, 'the persona credential is on the authenticator').toBeTruthy()
    merge(need('KOINON_DRILL_OIDC_SIGNCOUNT_OUT'), persona, { username, signCount: mine?.signCount })
  })
}

test('an authenticator without the credential does not sign in', async ({ page }) => {
  test.skip(process.env.KOINON_DRILL_OIDC_NEGATIVE !== '1', 'the negative case runs once, on the restored copy')
  await authenticator(page, null)
  await startSSO(page, 'admin').catch(() => undefined) // the issuer may refuse before the button exists
  await page.waitForTimeout(10_000)
  await expect(page.getByRole('heading', { name: 'Inbox', level: 1 })).toHaveCount(0)
  expect(new URL(page.url()).origin).not.toBe(new URL(need('KOINON_DRILL_WEB_URL')).origin)
})
