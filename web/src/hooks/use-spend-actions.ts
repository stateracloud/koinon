// SPDX-License-Identifier: Apache-2.0

import type { MessageInitShape } from '@bufbuild/protobuf'
import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { PriceService, type SetPriceRequestSchema } from '@/gen/koinon/budget/v1alpha1/price_pb'

/** usePriceActions returns the write calls of PriceService for one org; each refreshes the price lists. */
export function usePriceActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(PriceService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: PriceService, cardinality: undefined }) })
    return {
      setPrice: async (i: MessageInitShape<typeof SetPriceRequestSchema>) => {
        const res = await client.setPrice({ ...i, orgId })
        await refresh()
        return res
      },
      retirePrice: async (name: string) => {
        const res = await client.retirePrice({ orgId, name })
        await refresh()
        return res
      },
    }
  }, [transport, queryClient, orgId])
}

/** useBudgetActions returns the write calls of BudgetService for one org; each refreshes the budget lists. */
export function useBudgetActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(BudgetService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: BudgetService, cardinality: undefined }) })
    const done = async <T>(p: Promise<T>) => {
      const res = await p
      await refresh()
      return res
    }
    return {
      setMonthlyCap: (usd: string) => done(client.setMonthlyCap({ orgId, monthlyCapUsd: usd })),
      setPlatformCeiling: (usd: string) => done(client.setPlatformCeiling({ orgId, platformCeilingUsd: usd })),
      setAgentLimit: (agentId: string, usd: string) => done(client.setAgentBudget({ orgId, agentId, dailyLimitUsd: usd })),
      markRead: (notificationId: string) => done(client.markNotificationRead({ orgId, notificationId })),
    }
  }, [transport, queryClient, orgId])
}

/** usePlatformActions returns the write call of the platform area: a ceiling for any org. */
export function usePlatformActions() {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(BudgetService, transport)
    return {
      setCeiling: async (orgId: string, usd: string) => {
        const res = await client.setPlatformCeiling({ orgId, platformCeilingUsd: usd })
        await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: BudgetService, cardinality: undefined }) })
        return res
      },
    }
  }, [transport, queryClient])
}

/** usePlatformPriceActions returns the write calls of the platform price list (platform admins); each refreshes the price queries. */
export function usePlatformPriceActions() {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(PriceService, transport)
    const done = async <T>(p: Promise<T>) => {
      const res = await p
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: PriceService, cardinality: undefined }) })
      return res
    }
    return {
      accept: (id: string, confirm: string, maxOutput: number) => done(client.acceptPriceProposal({ id, confirm, maxOutput })),
      dismiss: (id: string) => done(client.dismissPriceProposal({ id })),
      rollback: (version: bigint) => done(client.rollbackPlatformPrices({ version })),
      checkSources: () => done(client.checkPriceSources({})),
    }
  }, [transport, queryClient])
}
