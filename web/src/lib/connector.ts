// SPDX-License-Identifier: Apache-2.0

// Reads a connector type's setup guide (ADR 0021) out of the document
// ListConnectorTypes returns. Its shape is connectors/schema/connector.schema.json;
// anything unexpected is dropped rather than trusted.

import type { ConnectorType } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { arr, obj, str } from '@/lib/json'

export type GuideField = { id: string; label: string; help?: string; type: string; secret: boolean; pattern?: string; required: boolean }
export type GuideLink = { label: string; url: string }
export type GuideValue = { label: string; value: string }
export type GuideStep = { id: string; title: string; body: string; links: GuideLink[]; values: GuideValue[]; fields: string[]; check?: string }
export type SetupGuide = { fields: GuideField[]; steps: GuideStep[]; targetPattern?: string }

/**
 * CodeHostInfo is what the descriptor of a code-host connector type (spec.capabilities, ADR 0062)
 * says about its credential: how to name it, and whether it is weaker than an App installation
 * token (a static bot token, ADR 0063), so the UI can say so.
 */
export type CodeHostInfo = {
  name: string
  /** "app-installation" or "static-token". */
  credentialClass: string
  /** A static token: not narrowed per repository or permission, and it does not expire. */
  weakerCredential: boolean
  /** Where to connect one: the connector's new-connection page. */
  connectPath: string
}

/** codeHostInfo reads the capability descriptor of a connector type; undefined for a connector that is not a code host. */
export function codeHostInfo(ct: ConnectorType | undefined): CodeHostInfo | undefined {
  const caps = obj(obj(ct?.document?.spec)?.capabilities)
  if (!ct || !caps) return undefined
  const credentialClass = str(caps.credentialClass) ?? ''
  return {
    name: str(caps.displayName) ?? ct.displayName ?? ct.id,
    credentialClass,
    weakerCredential: credentialClass === 'static-token',
    connectPath: `/connections/new/${ct.id}`,
  }
}

/** Only http(s) links are rendered; anything else is dropped. */
function safeUrl(u: string | undefined): string | undefined {
  if (!u) return undefined
  try {
    const p = new URL(u)
    return p.protocol === 'https:' || p.protocol === 'http:' ? u : undefined
  } catch {
    return undefined
  }
}

export function setupGuide(ct: ConnectorType | undefined): SetupGuide {
  const spec = obj(ct?.document?.spec)
  const guide = obj(spec?.setupGuide)
  const fields: GuideField[] = arr(guide?.fields).flatMap((f) => {
    const o = obj(f)
    const id = str(o?.id)
    if (!o || !id) return []
    return [
      {
        id,
        label: str(o.label) ?? id,
        help: str(o.help),
        type: str(o.type) ?? 'string',
        secret: o.secret === true,
        pattern: str(o.pattern),
        required: o.required !== false,
      },
    ]
  })
  const steps: GuideStep[] = arr(guide?.steps).flatMap((s) => {
    const o = obj(s)
    const id = str(o?.id)
    if (!o || !id) return []
    return [
      {
        id,
        title: str(o.title) ?? id,
        body: str(o.body) ?? '',
        links: arr(o.links).flatMap((l) => {
          const lo = obj(l)
          const url = safeUrl(str(lo?.url))
          return lo && url ? [{ label: str(lo.label) ?? url, url }] : []
        }),
        values: arr(o.values).flatMap((v) => {
          const vo = obj(v)
          const value = str(vo?.value)
          return vo && value !== undefined ? [{ label: str(vo.label) ?? '', value }] : []
        }),
        fields: arr(o.fields).flatMap((f) => (typeof f === 'string' ? [f] : [])),
        check: str(o.check),
      },
    ]
  })
  return { fields, steps, targetPattern: str(obj(spec?.targets)?.pattern) }
}
