// SPDX-License-Identifier: Apache-2.0

import { ConnectError, createClient } from '@connectrpc/connect'
import { useTransport } from '@connectrpc/connect-query'
import { type FormEvent, useEffect, useRef, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'

/** GitHub's limit for an App's name. */
const appNameMax = 34
const ownerPattern = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/

/**
 * GitHubAppSetup is the guided setup of the org's own GitHub App (issue #1175). Koinon builds the
 * App's manifest; the browser posts it to GitHub, which creates the App and sends the browser back.
 * The App's private key goes from GitHub to Koinon's secret store and is never shown here.
 */
export function GitHubAppSetup({ orgId }: { orgId: string }) {
  const transport = useTransport()
  const [name, setName] = useState('')
  const [owner, setOwner] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const [post, setPost] = useState<{ url: string; manifest: string }>()
  const form = useRef<HTMLFormElement>(null)

  // GitHub's "new App" page takes the manifest only as a posted form field.
  useEffect(() => {
    if (post) form.current?.submit()
  }, [post])

  const nameError = name.trim().length > appNameMax ? `The App's name is at most ${appNameMax} characters on GitHub.` : undefined
  const ownerError = owner.trim() && !ownerPattern.test(owner.trim()) ? 'Enter the login of a GitHub organization.' : undefined

  async function start(e: FormEvent) {
    e.preventDefault()
    if (!name.trim() || nameError || ownerError) return
    setPending(true)
    setError(undefined)
    try {
      const res = await createClient(ConnectionService, transport).startGitHubAppSetup({
        orgId,
        displayName: name.trim(),
        githubOwner: owner.trim(),
        baseUrl: baseUrl.trim(),
      })
      setPost({ url: res.postUrl, manifest: res.manifest })
    } catch (err) {
      setError(ConnectError.from(err).rawMessage)
      setPending(false)
    }
  }

  return (
    <section className="grid gap-3 rounded-md border p-4" aria-labelledby="github-app-setup-heading">
      <h2 id="github-app-setup-heading" className="font-medium">
        Create the org&apos;s own GitHub App
      </h2>
      <p className="text-muted-foreground text-sm">
        Koinon asks GitHub to create a private App with the permissions of the setup guide. Then you install the App on the repositories that agents
        can use. Pull requests that agents open show this App as the author, and each names the person who asked for it. The App&apos;s private key
        goes from GitHub to the secret store of Koinon and never to a sandbox.
      </p>
      <form className="grid gap-3" onSubmit={(e) => void start(e)}>
        <div className="grid gap-1">
          <Label htmlFor="app-name">App name</Label>
          <Input id="app-name" value={name} maxLength={appNameMax + 10} onChange={(e) => setName(e.target.value)} aria-invalid={!!nameError} required />
          <p className="text-muted-foreground text-xs">The name must be unique on GitHub. It is also the name of the connection.</p>
          {nameError && <p className="text-destructive text-sm">{nameError}</p>}
        </div>
        <div className="grid gap-1">
          <Label htmlFor="app-owner">GitHub organization (optional)</Label>
          <Input id="app-owner" value={owner} onChange={(e) => setOwner(e.target.value)} aria-invalid={!!ownerError} />
          <p className="text-muted-foreground text-xs">Leave it empty to create the App on your own GitHub account.</p>
          {ownerError && <p className="text-destructive text-sm">{ownerError}</p>}
        </div>
        <div className="grid gap-1">
          <Label htmlFor="app-base-url">GitHub Enterprise Server URL (optional)</Label>
          <Input id="app-base-url" value={baseUrl} placeholder="https://github.example.com" onChange={(e) => setBaseUrl(e.target.value)} />
        </div>
        {error && (
          <Alert variant="destructive" role="alert">
            <AlertDescription>Could not start the setup: {error}</AlertDescription>
          </Alert>
        )}
        <div>
          <Button type="submit" disabled={pending || !name.trim() || !!nameError || !!ownerError}>
            {pending ? 'Opening GitHub…' : 'Create the App on GitHub'}
          </Button>
        </div>
      </form>
      {post && (
        <form ref={form} method="post" action={post.url} hidden aria-hidden>
          <input type="hidden" name="manifest" value={post.manifest} />
        </form>
      )}
    </section>
  )
}
