// SPDX-License-Identifier: Apache-2.0

import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { forgetActingOrg } from '@/lib/org-storage'
import { logout } from '@/lib/session'

/** useSignOut ends the session, drops every cached query and goes to the login page, also when the call fails. */
export function useSignOut(): { signOut: () => void; signingOut: boolean } {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const [signingOut, setSigningOut] = useState(false)
  async function run() {
    setSigningOut(true)
    try {
      await logout()
    } finally {
      forgetActingOrg()
      queryClient.clear()
      await navigate({ to: '/login', search: {} })
    }
  }
  return { signOut: () => void run(), signingOut }
}
