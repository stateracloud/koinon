// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import {
  GiteaConnectionSecretSchema,
  GitHubConnectionSecretSchema,
  type SetConnectionSecretRequest,
  SetConnectionSecretRequestSchema,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'

/** The secret fields of a connection, as SetConnectionSecret takes them. */
export type ConnectionSecretInput = { connector: 'github'; privateKey: string } | { connector: 'gitea'; readToken: string; writeToken: string }

/** tokenError: a pasted token is one word; Gitea's are 40 hex characters but the format is not ours to enforce. */
export function tokenError(label: string, v: string): string | undefined {
  if (!v.trim()) return `${label} is required.`
  if (/\s/.test(v.trim())) return `${label} must be only the token, without spaces or line breaks.`
  if (v.length > 256) return `${label} is longer than 256 characters.`
  return undefined
}

/** privateKeyError checks that a GitHub App key is a pasted .pem file; the API parses it. */
export function privateKeyError(label: string, v: string): string | undefined {
  if (!v.trim()) return `${label} is required.`
  if (!/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(v)) return `${label} must be the contents of the .pem file.`
  return undefined
}

/** secretErrors validates every field of the input, keyed by field. */
export function secretErrors(input: ConnectionSecretInput): Record<string, string | undefined> {
  if (input.connector === 'github') return { privateKey: privateKeyError('Private key', input.privateKey) }
  const errors: Record<string, string | undefined> = {
    readToken: tokenError('Read token', input.readToken),
    writeToken: tokenError('Write token', input.writeToken),
  }
  if (!errors.readToken && !errors.writeToken && input.readToken.trim() === input.writeToken.trim()) {
    errors.writeToken = 'The write token must be a different token from the read token.'
  }
  return errors
}

/** setSecretRequest builds the request that stores the input and tests the connection. */
export function setSecretRequest(orgId: string, connectionId: string, input: ConnectionSecretInput): SetConnectionSecretRequest {
  const secret: SetConnectionSecretRequest['secret'] =
    input.connector === 'github'
      ? { case: 'github', value: create(GitHubConnectionSecretSchema, { privateKey: input.privateKey }) }
      : { case: 'gitea', value: create(GiteaConnectionSecretSchema, { readToken: input.readToken.trim(), writeToken: input.writeToken.trim() }) }
  return create(SetConnectionSecretRequestSchema, { orgId, connectionId, secret, test: true })
}
