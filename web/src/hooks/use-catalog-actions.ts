// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { TokenService } from '@/gen/koinon/token/v1alpha1/token_pb'
import type { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'

// These are plain client calls rather than useMutation: TanStack Query keeps a mutation's variables
// and result in its cache, and these carry provider keys and the token secret, which must not outlive
// the form that handled them.

export type ProviderCreate = { name: string; type: string; url: string; apiKey: string; anthropicWorkspaceId?: string; costApiKey?: string; costScope?: string }
export type ProviderChange = { type?: string; url?: string; apiKey?: string; anthropicWorkspaceId?: string; costApiKey?: string; costScope?: string }
/** format "" is the default; a value overrides it (ADR 0069). */
export type ModelCreate = { providerId: string; name: string; guard: boolean; format?: string }
/** format "" goes back to the default; unset leaves it. */
export type ModelChange = { guard?: boolean; format?: string }

/** useProviderActions returns the write calls of ProviderService for one org; each refreshes the lists. */
export function useProviderActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(ProviderService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ProviderService, cardinality: undefined }) })
    const done = async <T>(p: Promise<T>) => {
      const res = await p
      await refresh()
      return res
    }
    return {
      createProvider: (i: ProviderCreate) => done(client.createProvider({ orgId, ...i })),
      updateProvider: (providerId: string, i: ProviderChange) => done(client.updateProvider({ orgId, providerId, ...i })),
      deleteProvider: (providerId: string) => done(client.deleteProvider({ orgId, providerId })),
      createModel: (i: ModelCreate) => done(client.createModel({ orgId, ...i })),
      updateModel: (modelId: string, i: ModelChange) => done(client.updateModel({ orgId, modelId, ...i })),
      deleteModel: (modelId: string) => done(client.deleteModel({ orgId, modelId })),
    }
  }, [transport, queryClient, orgId])
}

/** useTokenActions returns create and revoke for one org's API tokens. */
export function useTokenActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(TokenService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TokenService, cardinality: undefined }) })
    return {
      create: async (i: { name: string; role: OrgRole; expiresInSeconds: bigint }) => {
        const res = await client.createToken({ orgId, name: i.name, role: i.role, expiresIn: { seconds: i.expiresInSeconds, nanos: 0 } })
        await refresh()
        return res
      },
      revoke: async (tokenId: string) => {
        const res = await client.revokeToken({ orgId, tokenId })
        await refresh()
        return res
      },
    }
  }, [transport, queryClient, orgId])
}
