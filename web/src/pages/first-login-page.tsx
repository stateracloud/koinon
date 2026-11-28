// SPDX-License-Identifier: Apache-2.0

import { Logo } from '@/components/logo'
import { Button } from '@/components/ui/button'
import { useSignOut } from '@/hooks/use-sign-out'

/**
 * FirstLoginPage is the screen of a user with no team org (ADR 0126 section 4, "First login"). It shows only the
 * ways in that exist today: an org admin adds the user. Invites, IdP-mapped orgs and "Create org" wait for their
 * decision (ADR 0126 section 7).
 */
export function FirstLoginPage({ name }: { name: string }) {
  const { signOut, signingOut } = useSignOut()
  return (
    <main className="flex min-h-screen items-center justify-center px-4 py-10">
      <div className="bg-card grid w-full max-w-md gap-4 rounded-lg border p-6">
        <Logo />
        <h1 className="text-xl font-semibold">{name ? `Welcome, ${name}` : 'Welcome'}</h1>
        <p className="text-muted-foreground text-sm">
          You are not a member of an org yet. An org admin must add you to their org. Then reload this page.
        </p>
        <div>
          <Button variant="outline" onClick={signOut} disabled={signingOut}>
            Sign out
          </Button>
        </div>
      </div>
    </main>
  )
}
