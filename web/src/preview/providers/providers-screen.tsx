// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Button, ButtonLink, CodeBlock, Dialog, DialogClose, PageHeader, PreviewShell, StatusBadge, usePaletteSelection } from '@/preview/ui'
import { providerTests, providers, type Provider } from '@/preview/mock/data'

type TestKey = keyof typeof providerTests

function initialTest(): TestKey | null {
  try {
    const t = new URLSearchParams(window.location.search).get('test')
    return t === 'ok' || t === 'bad' ? t : null
  } catch {
    return null
  }
}

export function ProvidersScreen() {
  const [which, setWhich] = useState<TestKey | null>(initialTest)
  const [picked, setPicked] = useState<string | null>(null)
  const t = which ? providerTests[which] : null

  usePaletteSelection(
    picked
      ? {
          key: picked,
          title: picked,
          heading: 'For the selected provider',
          actions: [{ label: 'Test connection', onSelect: () => setWhich(providers.find((p) => p.name === picked)?.test ?? 'ok') }],
        }
      : null,
  )

  return (
    <PreviewShell space="org" current="Providers" crumbs={[{ label: 'Providers' }]}>
      <PageHeader
        title="Providers"
        description="Where agents' model calls go. An org provider with the same name as a platform provider replaces it for this org; spend goes to this org's ledger either way."
        actions={
          <ButtonLink href="/preview/providers/add" variant="default">
            Add provider
          </ButtonLink>
        }
      />
      <div className="ks-panel-flush ks-table-wrap" role="region" aria-label="Providers" tabIndex={0}>
        <table className="ks-table">
          <thead>
            <tr>
              <th scope="col">Provider</th>
              <th scope="col">Source</th>
              <th scope="col">Credential</th>
              <th scope="col" className="ks-r">Models</th>
              <th scope="col">Status</th>
              <th scope="col" className="ks-r">This month</th>
              <th scope="col">
                <span className="ks-sr">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {providers.map((p) => (
              <ProviderRow key={p.name} p={p} selected={picked === p.name} onPick={() => setPicked(p.name)} onTest={() => setWhich(p.test)} />
            ))}
          </tbody>
        </table>
      </div>
      <p className="ks-p ks-meta">Keys are write-only: Koinon never shows them again. Rotate replaces a key; running agents pick it up on their next run.</p>

      <Dialog
        open={t !== null}
        onOpenChange={(o) => !o && setWhich(null)}
        size="wide"
        title={
          t && (
            <span className="ks-between" style={{ gap: 12 }}>
              {t.title}
              <StatusBadge kind="connection" state={t.state} />
            </span>
          )
        }
        footer={
          <>
            <Button variant="outline" onClick={() => setWhich(which === 'ok' ? 'bad' : 'ok')}>
              {which === 'ok' ? 'Show the failing test' : 'Show the passing test'}
            </Button>
            <DialogClose>
              <Button>Close</Button>
            </DialogClose>
          </>
        }
      >
        {t && (
          <>
            <dl className="ks-kv">
              <dt>Echo check</dt>
              <dd className={t.passed ? 'ks-ok' : 'ks-err'}>{t.echo}</dd>
              <dt>Latency</dt>
              <dd className="ks-num">{t.latency}</dd>
              <dt>Cost of the test</dt>
              <dd className="ks-num">{t.cost}</dd>
              <dt>Models found</dt>
              <dd>{t.models}</dd>
            </dl>
            <div className="ks-stack">
              <span className="ks-label">Request sent</span>
              <CodeBlock tabIndex={0}>{t.request}</CodeBlock>
            </div>
            <div className="ks-stack">
              <span className="ks-label">Raw response</span>
              <CodeBlock tabIndex={0}>{t.response}</CodeBlock>
            </div>
            <p className="ks-p ks-meta">The key is sent by agentgateway; it never appears here or in a sandbox.</p>
          </>
        )}
      </Dialog>
    </PreviewShell>
  )
}

function ProviderRow({ p, selected, onPick, onTest }: { p: Provider; selected: boolean; onPick: () => void; onTest: () => void }) {
  return (
    <tr aria-selected={selected} onClick={onPick}>
      <td>
        <span className="ks-stack" style={{ gap: 0 }}>
          <span style={{ fontWeight: 500 }}>{p.name}</span>
          <span className="ks-meta">{p.kind}</span>
        </span>
      </td>
      <td>
        <span className={p.source === 'Inherited from Platform' ? 'kn-badge kn-badge-secondary' : 'kn-badge kn-badge-outline'}>{p.source}</span>
      </td>
      <td>
        <span className="ks-stack" style={{ gap: 0 }}>
          <span className="ks-mono">{p.credential} ••••</span>
          <span className="ks-meta">lives in: {p.lives}</span>
        </span>
      </td>
      <td className="ks-r">{p.models}</td>
      <td>
        <StatusBadge kind="connection" state={p.state} />
      </td>
      <td className="ks-r">{p.spend}</td>
      <td className="ks-r">
        <Button
          variant="outline"
          size="xs"
          aria-label={`Test ${p.name}`}
          onClick={(e) => {
            e.stopPropagation()
            onPick()
            onTest()
          }}
        >
          Test
        </Button>
      </td>
    </tr>
  )
}
