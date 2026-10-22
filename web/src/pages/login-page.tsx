// SPDX-License-Identifier: Apache-2.0

import { useQuery as useTanstackQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { whoAmIKey } from '@/lib/query'
import { fetchLoginMethods, login, oidcStartURL, safeReturnPath } from '@/lib/session'

export function LoginPage() {
  const { redirect } = useSearch({ from: '/login' })
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const methods = useTanstackQuery({ queryKey: ['auth-methods'], queryFn: fetchLoginMethods, staleTime: 60_000 })
  const [username, setUsername] = useState('admin')
  const [password, setPassword] = useState('')
  const [problem, setProblem] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setProblem(null)
    try {
      await login(username, password)
      await queryClient.resetQueries({ queryKey: whoAmIKey })
      // The app layout shows the password change form first if one is due.
      await navigate({ to: safeReturnPath(redirect) })
    } catch (err) {
      setProblem(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const showPassword = methods.data?.password ?? true
  return (
    <main className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            <h1>Sign in to Koinon</h1>
          </CardTitle>
          {showPassword && <CardDescription>Use the bootstrap admin account, or single sign-on.</CardDescription>}
        </CardHeader>
        <CardContent className="grid gap-4">
          {methods.data?.oidc && (
            <Button asChild variant="default">
              <a href={oidcStartURL(redirect)}>Sign in with SSO</a>
            </Button>
          )}
          {showPassword && (
            <form onSubmit={(e) => void onSubmit(e)} className="grid gap-4">
              <div className="grid gap-2">
                <Label htmlFor="username">Username</Label>
                <Input id="username" autoComplete="username" required value={username} onChange={(e) => setUsername(e.target.value)} />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="password">Password</Label>
                <Input id="password" type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
              </div>
              {problem && (
                <Alert variant="destructive" role="alert">
                  <AlertDescription>{problem}</AlertDescription>
                </Alert>
              )}
              <Button type="submit" variant={methods.data?.oidc ? 'outline' : 'default'} disabled={busy}>
                Sign in
              </Button>
            </form>
          )}
          {methods.data && !methods.data.password && !methods.data.oidc && (
            <p role="alert">No login method is enabled. Ask your operator to configure OIDC.</p>
          )}
        </CardContent>
      </Card>
    </main>
  )
}
