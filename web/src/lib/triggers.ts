// SPDX-License-Identifier: Apache-2.0

import { NotifyTargetKind, TriggerType, type Trigger } from '@/gen/koinon/template/v1alpha1/template_pb'

/** triggerProblem returns what is wrong with a trigger the server would refuse, if anything. */
export function triggerProblem(t: Trigger): string | undefined {
  if (!/^[a-z][a-z0-9]*(-[a-z0-9]+)*$/.test(t.id) || t.id.length > 63) return 'The name must be lowercase letters, digits and dashes.'
  if (t.instruction.trim() === '') return 'The instruction is required.'
  if (t.type === TriggerType.SCHEDULE) {
    if (t.cron.trim().split(/\s+/).length !== 5) return 'The schedule needs five cron fields.'
    if (t.timezone.trim() === '') return 'The schedule needs a time zone.'
  }
  if (t.notify?.kind === NotifyTargetKind.USER && t.notify.userId === '') return 'Pick the person who is asked.'
  return undefined
}
