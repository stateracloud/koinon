// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  CheckResultSchema,
  type Connection,
  ConnectionSchema,
  ConnectionService,
  ConnectionStatus,
  GiteaConnectionSchema,
  GitHubConnectionSchema,
  type SetConnectionSecretRequest,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { giteaConnectorType, githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

// Obviously fake secrets (AGENTS.md).
const pemKey = '-----BEGIN RSA PRIVATE KEY-----\nkoinon_test_not-a-real-key\n-----END RSA PRIVATE KEY-----\n'
const readTok = 'koinon_test_not-a-real-read-token'
const writeTok = 'koinon_test_not-a-real-write-token'

// Overrides of a test connection: any field but the message's own brand.
type ConnectionOverrides = Partial<Omit<Connection, '$typeName' | '$unknown'>>

function github(over: ConnectionOverrides = {}) {
  return create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'github',
    displayName: 'Acme GitHub',
    status: ConnectionStatus.UNTESTED,
    source: 'ui',
    settings: { case: 'github', value: create(GitHubConnectionSchema, { appId: 12n, installationId: 34n }) },
    ...over,
  })
}

function gitea(over: ConnectionOverrides = {}) {
  return create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'gitea',
    displayName: 'Lab Gitea',
    status: ConnectionStatus.OK,
    source: 'ui',
    settings: { case: 'gitea', value: create(GiteaConnectionSchema, { baseUrl: 'https://example.test/gitea' }) },
    ...over,
  })
}

function setup(conn: Connection, opts: { role?: OrgRole; fail?: string } = {}) {
  const sets: SetConnectionSecretRequest[] = []
  let current = conn
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType(), giteaConnectorType()] }),
      getConnection: () => ({ connection: current }),
      listInstallationRepos: () => ({ repos: [], totalCount: 0n }),
      setConnectionSecret: (req) => {
        sets.push(req)
        if (opts.fail) throw new ConnectError(opts.fail, Code.InvalidArgument)
        const results = [create(CheckResultSchema, { check: 'secret', step: 'x', ok: true, message: 'ok' })]
        current = create(ConnectionSchema, { ...current, credentialLostAt: undefined, status: ConnectionStatus.OK, lastTest: results })
        return { connection: current, results }
      },
    })
  })
  renderApp(transport, '/connections/c-1')
  return sets
}

describe('ConnectionPage secret re-entry', () => {
  it('opens the form for a lost private key, sends it with a test, and clears it', async () => {
    const sets = setup(github({ credentialLostAt: timestampFromDate(new Date()) }))
    expect(await screen.findByRole('note')).toHaveTextContent('private key was lost with a key-encryption key')
    expect(screen.getByRole('note')).toHaveTextContent('Enter it again below.')
    expect(screen.getByText('Enter the private key again')).toBeInTheDocument()

    const field = screen.getByLabelText('Private key')
    await userEvent.click(screen.getByRole('button', { name: 'Store and test' }))
    expect(await screen.findByText('Private key is required.')).toBeInTheDocument()
    expect(sets).toHaveLength(0)

    await userEvent.click(field)
    await userEvent.paste(pemKey)
    await userEvent.click(screen.getByRole('button', { name: 'Store and test' }))
    expect(await screen.findByText('Stored. All checks passed.')).toBeInTheDocument()
    expect(sets).toHaveLength(1)
    expect(sets[0].connectionId).toBe('c-1')
    expect(sets[0].test).toBe(true)
    expect(sets[0].secret.case).toBe('github')
    expect(sets[0].secret.case === 'github' && sets[0].secret.value.privateKey).toBe(pemKey)
    expect(screen.getByLabelText('Private key')).toHaveValue('')
    expect(document.body.textContent).not.toContain('not-a-real')
  })

  it('replaces both Gitea tokens, trimmed, and refuses the same token twice', async () => {
    const sets = setup(gitea())
    await userEvent.click(await screen.findByText('Replace the tokens'))
    await userEvent.click(screen.getByLabelText('Read token'))
    await userEvent.paste(` ${readTok} `)
    await userEvent.click(screen.getByLabelText('Write token'))
    await userEvent.paste(readTok)
    await userEvent.click(screen.getByRole('button', { name: 'Store and test' }))
    expect(await screen.findByText('The write token must be a different token from the read token.')).toBeInTheDocument()
    expect(sets).toHaveLength(0)

    await userEvent.clear(screen.getByLabelText('Write token'))
    await userEvent.click(screen.getByLabelText('Write token'))
    await userEvent.paste(writeTok)
    await userEvent.click(screen.getByRole('button', { name: 'Store and test' }))
    expect(await screen.findByText('Stored. All checks passed.')).toBeInTheDocument()
    expect(sets[0].secret).toMatchObject({ case: 'gitea', value: { readToken: readTok, writeToken: writeTok } })
    expect(screen.getByLabelText('Read token')).toHaveValue('')
    expect(screen.getByLabelText('Write token')).toHaveValue('')
  })

  it('shows the refusal, and the secret stays cleared', async () => {
    setup(github(), { fail: 'private_key: not a PEM private key' })
    await userEvent.click(await screen.findByText('Replace the private key'))
    await userEvent.click(screen.getByLabelText('Private key'))
    await userEvent.paste(pemKey)
    await userEvent.click(screen.getByRole('button', { name: 'Store and test' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not store the private key: private_key: not a PEM private key')
    expect(screen.getByLabelText('Private key')).toHaveValue('')
    expect(screen.getByText(/sent and cleared from this page/)).toBeInTheDocument()
  })

  it('offers no form for a declared connection, which comes back from its Secret', async () => {
    setup(github({ source: 'secret', managedBySecret: 'koinon/gh', credentialLostAt: timestampFromDate(new Date()) }))
    expect(await screen.findByRole('note')).toHaveTextContent('Koinon reads it again from the Secret koinon/gh on its next sync.')
    expect(screen.queryByText(/the private key again/)).not.toBeInTheDocument()
  })

  it('offers no form to members', async () => {
    setup(github({ credentialLostAt: timestampFromDate(new Date()) }), { role: OrgRole.MEMBER })
    expect(await screen.findByRole('note')).toBeInTheDocument()
    expect(screen.queryByText(/the private key again/)).not.toBeInTheDocument()
  })
})
