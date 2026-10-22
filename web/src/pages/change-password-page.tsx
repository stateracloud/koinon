// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { whoAmIKey } from '@/lib/query'

const minLength = 12

/** ChangePasswordPage replaces the bootstrap admin's one-time password. */
export function ChangePasswordPage() {
  const queryClient = useQueryClient()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [problem, setProblem] = useState<string | null>(null)
  const change = useMutation(AuthService.method.changePassword, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: whoAmIKey }),
    onError: (err) => setProblem(ConnectError.from(err).rawMessage),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (next.length < minLength) return setProblem(`The new password needs at least ${minLength} characters.`)
    if (next === current) return setProblem('The new password must differ from the current one.')
    if (next !== confirm) return setProblem('The new passwords do not match.')
    setProblem(null)
    change.mutate({ currentPassword: current, newPassword: next })
  }

  return (
    <main className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>
            <h1>Choose a new password</h1>
          </CardTitle>
          <CardDescription>The one-time bootstrap password must be replaced before you continue.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-4" noValidate>
            <div className="grid gap-2">
              <Label htmlFor="current-password">Current password</Label>
              <Input id="current-password" type="password" autoComplete="current-password" required value={current} onChange={(e) => setCurrent(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="new-password">New password</Label>
              <Input id="new-password" type="password" autoComplete="new-password" required minLength={minLength} aria-describedby="new-password-hint" value={next} onChange={(e) => setNext(e.target.value)} />
              <p id="new-password-hint" className="text-muted-foreground text-xs">
                At least {minLength} characters.
              </p>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="confirm-password">Repeat new password</Label>
              <Input id="confirm-password" type="password" autoComplete="new-password" required value={confirm} onChange={(e) => setConfirm(e.target.value)} />
            </div>
            {problem && (
              <Alert variant="destructive" role="alert">
                <AlertDescription>{problem}</AlertDescription>
              </Alert>
            )}
            <Button type="submit" disabled={change.isPending}>
              Change password
            </Button>
          </form>
        </CardContent>
      </Card>
    </main>
  )
}
