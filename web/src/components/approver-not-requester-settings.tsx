// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { describeError } from '@/lib/api-error'

/**
 * ApproverNotRequesterSettings is where an org admin turns on the optional rule "the approver must not
 * be the requester" (issue #1175, ADR 0005 amendment 2026-11-23). It is off by default. A
 * second-person rule on the code host stays the org's own code-host setting. Every change is audited.
 */
export function ApproverNotRequesterSettings({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(OrgSettingsService.method.getApproverNotRequester, { orgId })
  const save = useMutation(OrgSettingsService.method.setApproverNotRequester, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: OrgSettingsService, cardinality: undefined }) }),
  })

  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the approval rule: {describeError(query.error).message}</AlertDescription>
      </Alert>
    )
  }
  if (!query.data) return <p role="status">Loading the approval rule…</p>
  const on = query.data.on

  return (
    <section aria-labelledby="approver-not-requester" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <h2 id="approver-not-requester" className="font-semibold">
        Approvals
      </h2>
      <label className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={on}
          disabled={save.isPending}
          onChange={(e) => save.mutate({ orgId, on: e.target.checked })}
          aria-describedby="approver-not-requester-help"
        />
        The approver must not be the requester
      </label>
      <p id="approver-not-requester-help" className="text-muted-foreground text-xs">
        When this is on, the person who started a task cannot approve the actions of that task, and an approval for later calls does not cover the tasks of the person who gave it. A
        second-person rule on GitHub, such as two approvals on a protected branch, is a setting of your GitHub organization. Koinon does not change it.
      </p>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {describeError(save.error).message}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
