// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { ConnectionSchema, ConnectionStatus, ConnectorTypeSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { GrantSchema, Preset, RunLimitsSchema, TemplateSchema } from '@/gen/koinon/template/v1alpha1/template_pb'
import { orgId } from '@/test-utils'

export function repoHelperTemplate(targets: { min: number; max: number } = { min: 1, max: 1 }) {
  return create(TemplateSchema, {
    id: 'tpl-1',
    slug: 'repo-helper',
    version: '0.1.0',
    displayName: 'Repo helper',
    description: 'Makes small changes to one repository.',
    allowedHarnesses: ['opencode'],
    defaultHarness: 'opencode',
    model: 'default',
    grants: [
      create(GrantSchema, {
        connector: 'github',
        scopes: ['repo.read', 'pull_request.create', 'issue.comment'],
        preset: Preset.STANDARD,
        targetsMin: targets.min,
        targetsMax: targets.max,
        purpose: 'Clone and read the repository; propose pull requests.',
      }),
    ],
    runLimits: create(RunLimitsSchema, { maxDurationSeconds: 1800n, maxModelRequests: 300n, maxSpendUsd: '5.00' }),
    spendLimitUsd: '10.00',
  })
}

export function githubConnection(id = 'c-1', name = 'Acme GitHub') {
  return create(ConnectionSchema, { id, orgId, connector: 'github', displayName: name, status: ConnectionStatus.OK })
}

/** A cut-down GitHub connector type in the shape of connectors/github/connector.yaml. */
export function githubConnectorType() {
  return create(ConnectorTypeSchema, {
    id: 'github',
    version: '0.2.0',
    displayName: 'GitHub',
    authType: 'app-credential',
    document: {
      spec: {
        capabilities: { displayName: 'GitHub', credentialClass: 'app-installation', narrowsRepositories: true, narrowsPermissions: true },
        targets: { kind: 'repository', pattern: '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$' },
        setupGuide: {
          fields: [
            { id: 'appId', label: 'App ID', type: 'integer', pattern: '^[1-9][0-9]{0,18}$' },
            { id: 'installationId', label: 'Installation ID', type: 'integer', pattern: '^[1-9][0-9]{0,18}$' },
            { id: 'privateKey', label: 'Private key', type: 'pem', secret: true },
          ],
          steps: [
            {
              id: 'create-app',
              title: 'Create a GitHub App',
              body: 'Set **Contents** to read and write.\n\n- Issues: **Read and write**\n- Workflows: none',
              links: [
                { label: 'New GitHub App', url: 'https://github.com/settings/apps/new' },
                { label: 'Bad link', url: 'javascript:alert(1)' },
              ],
              fields: ['appId'],
            },
            { id: 'generate-key', title: 'Generate a private key', body: 'Paste the `.pem` file.', fields: ['privateKey'], check: 'github.app' },
            { id: 'install-app', title: 'Install the App', body: 'Install it.', fields: ['installationId'], check: 'github.installation' },
            { id: 'test-token', title: 'Test the connection', body: 'Koinon mints a token.', check: 'github.token' },
          ],
        },
      },
    },
  })
}

/** Not a key: only the shape of a PEM block, for the secure-input tests. */
/** A cut-down Gitea connector type in the shape of connectors/gitea/connector.yaml. */
export function giteaConnectorType() {
  return create(ConnectorTypeSchema, {
    id: 'gitea',
    version: '0.1.0',
    displayName: 'Gitea',
    authType: 'static-token',
    document: {
      spec: {
        capabilities: { displayName: 'Gitea', credentialClass: 'static-token', writes: 'push' },
        targets: { kind: 'repository', pattern: '^[A-Za-z0-9_][A-Za-z0-9_.-]{0,39}/[A-Za-z0-9._-]{1,100}$' },
        setupGuide: {
          fields: [
            { id: 'readToken', label: 'Read token', type: 'text', secret: true },
            { id: 'writeToken', label: 'Write token', type: 'text', secret: true },
          ],
          steps: [
            { id: 'create-bot', title: 'Create a bot account', body: 'Create a user for Koinon.', check: 'gitea.server' },
            { id: 'add-collaborator', title: 'Add the bot to your repositories', body: 'Give it **Write** permission.' },
            {
              id: 'create-tokens',
              title: 'Create the two access tokens',
              body: 'Read token: `read:repository`.',
              links: [{ label: 'Gitea docs, token scopes', url: 'https://docs.gitea.com/usage/access-token-scopes' }],
              fields: ['readToken', 'writeToken'],
              check: 'gitea.tokens',
            },
            { id: 'test-repositories', title: 'Check what the bot can see', body: 'Koinon lists them.', check: 'gitea.repositories' },
            { id: 'test-read-token', title: 'Check that the read token cannot write', body: 'Koinon tries.', check: 'gitea.read-token' },
          ],
        },
      },
    },
  })
}

export function giteaConnection(id = 'c-2', name = 'Lab Gitea') {
  return create(ConnectionSchema, { id, orgId, connector: 'gitea', displayName: name, status: ConnectionStatus.OK })
}

/** The repo-helper template on a Gitea connection. */
export function repoHelperGiteaTemplate() {
  const t = repoHelperTemplate()
  return create(TemplateSchema, { ...t, id: 'tpl-2', slug: 'repo-helper-gitea', displayName: 'Repo helper (Gitea)', grants: t.grants.map((g) => create(GrantSchema, { ...g, connector: 'gitea' })) })
}

export const fakePem = '-----BEGIN RSA PRIVATE KEY-----\nTEST-ONLY-NOT-A-KEY\n-----END RSA PRIVATE KEY-----'
