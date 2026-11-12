// SPDX-License-Identifier: Apache-2.0

import type { SandboxRange } from '@/gen/koinon/agent/v1alpha1/agent_pb'

/** boundsText words a platform range: "512Mi to 8Gi", "at most 4" or "any value". */
export function boundsText(r?: SandboxRange): string {
  if (!r || (!r.min && !r.max)) return 'any value'
  if (r.min && r.max) return `${r.min} to ${r.max}`
  return r.min ? `at least ${r.min}` : `at most ${r.max}`
}
