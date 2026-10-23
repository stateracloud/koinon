// SPDX-License-Identifier: Apache-2.0

import { CircleCheck, CircleX, ExternalLink } from 'lucide-react'
import type { ReactNode } from 'react'
import { GuideText } from '@/components/guide-text'
import type { CheckResult } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { GuideStep } from '@/lib/connector'

export type SetupGuideStepsProps = {
  steps: readonly GuideStep[]
  /** Renders a step's input fields (the new-connection form). */
  renderFields?: (step: GuideStep) => ReactNode
  /** Check results, shown at the step they validate. */
  results?: readonly CheckResult[]
}

/** SetupGuideSteps walks a connector type's setup guide, step by step (ADR 0021). */
export function SetupGuideSteps({ steps, renderFields, results = [] }: SetupGuideStepsProps) {
  return (
    <ol className="grid gap-4" aria-label="Setup steps">
      {steps.map((s, i) => {
        const stepResults = results.filter((r) => r.step === s.id)
        const headingId = `step-${s.id}`
        return (
          <li key={s.id} aria-labelledby={headingId} className="bg-card grid gap-3 rounded-lg border p-4">
            <h2 id={headingId} className="font-semibold">
              {i + 1}. {s.title}
            </h2>
            <GuideText text={s.body} />
            {s.values.length > 0 && (
              <dl className="grid gap-1 text-sm">
                {s.values.map((v) => (
                  <div key={v.label} className="flex flex-wrap gap-2">
                    <dt className="text-muted-foreground">{v.label}</dt>
                    <dd>
                      <code className="bg-muted rounded px-1 text-xs select-all">{v.value}</code>
                    </dd>
                  </div>
                ))}
              </dl>
            )}
            {s.links.length > 0 && (
              <ul className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
                {s.links.map((l) => (
                  <li key={l.url}>
                    <a href={l.url} target="_blank" rel="noreferrer noopener" className="inline-flex items-center gap-1 underline">
                      {l.label}
                      <ExternalLink aria-hidden className="size-3" />
                    </a>
                  </li>
                ))}
              </ul>
            )}
            {renderFields?.(s)}
            {stepResults.length > 0 && (
              <ul className="grid gap-1 text-sm" aria-label={`Checks for ${s.title}`}>
                {stepResults.map((r) => (
                  <li key={r.check} className="flex items-start gap-2" data-ok={r.ok}>
                    {r.ok ? <CircleCheck aria-hidden className="mt-0.5 size-4 text-green-600" /> : <CircleX aria-hidden className="text-destructive mt-0.5 size-4" />}
                    <span>
                      <span className="font-medium">{r.ok ? 'Passed' : 'Failed'}</span>
                      {r.message && `: ${r.message}`} <span className="text-muted-foreground font-mono text-xs">({r.check})</span>
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </li>
        )
      })}
    </ol>
  )
}
