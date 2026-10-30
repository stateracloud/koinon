// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Alert, AlertDescription, AlertTitle, Button, ButtonLink, CodeBlock, Dialog, DialogClose, Field, Input, NativeSelect, PreviewShell, Segmented, StatusBadge, usePreviewNavigate } from '@/preview/ui'
import { providerTests } from '@/preview/mock/data'
import { discoveredModels, platformProviderNames, providerKinds, savedCredentials, type ProviderKindName } from './data'

type CredMode = 'saved' | 'new'
type Lives = 'gateway' | 'openshell'

export function ProviderAddScreen() {
  const navigate = usePreviewNavigate()
  const [kind, setKind] = useState<ProviderKindName>('Anthropic')
  const [name, setName] = useState('anthropic')
  const [workspace, setWorkspace] = useState('wrkspc_01JBK7Q3PLATFORMENG')
  const [baseUrl, setBaseUrl] = useState('https://api.anthropic.com')
  const [credMode, setCredMode] = useState<CredMode>('new')
  const [credName, setCredName] = useState('anthropic-pe')
  const [saved, setSaved] = useState(savedCredentials[0])
  const [apiKey, setApiKey] = useState('koinon_test_not-a-real-token')
  const [lives, setLives] = useState<Lives>('gateway')
  const [testState, setTestState] = useState<'idle' | 'running' | 'done'>('done')
  const [enabled, setEnabled] = useState<Record<string, boolean>>(() => Object.fromEntries(discoveredModels.map((m) => [m.id, m.on])))
  const [showTest, setShowTest] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])

  const replaces = platformProviderNames.includes(name.trim())
  const anthropic = kind === 'Anthropic'

  function pickKind(k: (typeof providerKinds)[number]) {
    setKind(k.name)
    setBaseUrl(k.baseUrl)
    setTestState('idle')
  }
  function runTest() {
    setTestState('running')
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setTestState('done'), 700)
  }
  function submit(e: FormEvent) {
    e.preventDefault()
    navigate('/preview/providers')
  }

  return (
    <PreviewShell space="org" current="Providers" crumbs={[{ label: 'Providers', href: '/preview/providers' }, { label: 'Add' }]} narrow bare>
      <form className="ks-page ks-page-narrow" aria-labelledby="pa-h" onSubmit={submit}>
        <h1 id="pa-h" className="ks-h1">
          Add a provider
        </h1>

        <section className="ks-panel ks-stack-3" aria-labelledby="pa-kind-h">
          <h2 id="pa-kind-h" className="ks-h4">
            Kind
          </h2>
          <div className="ks-grid-4" style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(150px, 1fr))', gap: 8 }}>
            {providerKinds.map((k) => (
              <button key={k.name} type="button" className="ks-card-a" aria-pressed={k.name === kind} onClick={() => pickKind(k)} style={{ padding: 12, gap: 2 }}>
                <span style={{ fontWeight: 600 }}>{k.name}</span>
                <span className="ks-meta">{k.format}</span>
              </button>
            ))}
          </div>
        </section>

        <section className="ks-panel ks-stack-4" aria-labelledby="pa-conn-h">
          <h2 id="pa-conn-h" className="ks-h4">
            Connection
          </h2>
          <div className="ks-grid-2" style={{ gap: 12 }}>
            <Field id="pa-name" label="Name">
              <Input id="pa-name" className="ks-mono" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" spellCheck={false} />
            </Field>
            {anthropic && (
              <Field id="pa-ws" label="Workspace ID" hint="Spend shows under this workspace in the Anthropic console.">
                <Input id="pa-ws" className="ks-mono" value={workspace} onChange={(e) => setWorkspace(e.target.value)} aria-describedby="pa-ws-hint" autoComplete="off" spellCheck={false} />
              </Field>
            )}
          </div>
          {replaces && (
            <Alert variant="info" role="status">
              <AlertTitle>This replaces the platform&apos;s {name.trim()} for this org</AlertTitle>
              <AlertDescription>The platform has a provider with the same name. Once saved, agents in Platform Engineering use this one, and its badge reads Org override.</AlertDescription>
            </Alert>
          )}
          <Field id="pa-url" label="Base URL">
            <Input id="pa-url" className="ks-mono" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://" autoComplete="off" spellCheck={false} />
          </Field>
        </section>

        <section className="ks-panel ks-stack-4" aria-labelledby="pa-cred-h">
          <h2 id="pa-cred-h" className="ks-h4">
            Credential
          </h2>
          <Segmented<CredMode>
            label="Credential"
            value={credMode}
            onChange={setCredMode}
            options={[
              { id: 'saved', label: 'Use a saved credential' },
              { id: 'new', label: 'New credential' },
            ]}
          />
          {credMode === 'new' ? (
            <div className="ks-grid-2" style={{ gap: 12 }}>
              <Field id="pa-cn" label="Credential name" hint="Reusable by other providers and MCP servers in this org.">
                <Input id="pa-cn" className="ks-mono" value={credName} onChange={(e) => setCredName(e.target.value)} aria-describedby="pa-cn-hint" autoComplete="off" spellCheck={false} />
              </Field>
              <Field id="pa-key" label="API key" hint="Write-only. After saving you can rotate it, never read it.">
                <Input id="pa-key" className="ks-mono" type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} aria-describedby="pa-key-hint" autoComplete="off" />
              </Field>
            </div>
          ) : (
            <Field id="pa-saved" label="Saved credential" hint="Only the name is shown; the value is never readable.">
              <NativeSelect id="pa-saved" className="ks-mono" value={saved} onChange={(e) => setSaved(e.target.value)} aria-describedby="pa-saved-hint">
                {savedCredentials.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </NativeSelect>
            </Field>
          )}
          <fieldset className="ks-stack" style={{ border: 0, padding: 0, margin: 0 }}>
            <legend className="kn-label" style={{ marginBottom: 6 }}>
              Where it lives
            </legend>
            <label className="ks-row">
              <input type="radio" name="lives" checked={lives === 'gateway'} onChange={() => setLives('gateway')} /> agentgateway <span className="ks-meta">· the gateway adds it to each model call</span>
            </label>
            <label className="ks-row">
              <input type="radio" name="lives" checked={lives === 'openshell'} onChange={() => setLives('openshell')} /> OpenShell provider record <span className="ks-meta">· for harnesses that call the provider directly</span>
            </label>
            <span className="ks-meta">Never in a sandbox, an image, a home volume or a log.</span>
          </fieldset>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="pa-test-h">
          <div className="ks-between">
            <h2 id="pa-test-h" className="ks-h4">
              Test before saving
            </h2>
            <Button variant="outline" size="sm" onClick={runTest} disabled={testState === 'running'}>
              {testState === 'running' ? 'Testing…' : 'Test connection'}
            </Button>
          </div>
          {testState === 'done' && (
            <div className="ks-raised ks-stack" role="status" style={{ padding: 12 }}>
              <span className="ks-row">
                <StatusBadge kind="connection" state="ok" />
                <span>Echo check matched in 388 ms · cost $0.00003</span>
                <button type="button" className="ks-link" style={{ marginLeft: 'auto', border: 0, background: 'none', padding: 0, font: 'inherit', cursor: 'pointer' }} onClick={() => setShowTest(true)}>
                  See request and response
                </button>
              </span>
            </div>
          )}
          <p className="ks-p ks-meta">A failing test warns but doesn&apos;t block: you can save and fix the key later.</p>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="pa-models-h">
          <div className="ks-between">
            <h2 id="pa-models-h" className="ks-h4">
              Models
            </h2>
            <span className="ks-meta">From /v1/models, filtered to Koinon&apos;s price catalog</span>
          </div>
          {testState === 'done' && anthropic ? (
            <div className="ks-table-wrap" role="region" aria-label="Discovered models" tabIndex={0}>
              <table className="ks-table">
                <thead>
                  <tr>
                    <th scope="col">
                      <span className="ks-sr">Enable</span>
                    </th>
                    <th scope="col">Model</th>
                    <th scope="col">Format</th>
                    <th scope="col" className="ks-r">Context</th>
                    <th scope="col">Catalog</th>
                  </tr>
                </thead>
                <tbody>
                  {discoveredModels.map((m) => {
                    const priced = m.catalog !== null
                    return (
                      <tr key={m.id}>
                        <td>
                          <input
                            type="checkbox"
                            checked={priced && enabled[m.id]}
                            disabled={!priced}
                            onChange={(e) => setEnabled({ ...enabled, [m.id]: e.target.checked })}
                            aria-label={priced ? `Enable ${m.id}` : `${m.id} is not in the catalog`}
                          />
                        </td>
                        <td className={priced ? 'ks-mono' : 'ks-mono ks-muted'}>{m.id}</td>
                        <td className={priced ? undefined : 'ks-muted'}>{m.format}</td>
                        <td className={priced ? 'ks-r' : 'ks-r ks-muted'}>{m.context}</td>
                        <td>{priced ? <span className="kn-badge kn-badge-secondary">{m.catalog}</span> : <span className="ks-meta">Not in the catalog; can&apos;t be priced</span>}</td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="ks-p ks-muted">{testState === 'running' ? 'Listing models…' : 'Test the connection to list the models this provider offers.'}</p>
          )}
        </section>

        <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
          <ButtonLink href="/preview/providers" variant="outline">
            Cancel
          </ButtonLink>
          <Button type="submit">Save provider</Button>
        </div>
      </form>

      <Dialog
        open={showTest}
        onOpenChange={setShowTest}
        size="wide"
        title="Test of the new provider passed"
        footer={
          <DialogClose>
            <Button>Close</Button>
          </DialogClose>
        }
      >
        <div className="ks-stack">
          <span className="ks-label">Request sent</span>
          <CodeBlock tabIndex={0}>{providerTests.ok.request}</CodeBlock>
        </div>
        <div className="ks-stack">
          <span className="ks-label">Raw response</span>
          <CodeBlock tabIndex={0}>{providerTests.ok.response}</CodeBlock>
        </div>
        <p className="ks-p ks-meta">The key is sent by agentgateway; it never appears here or in a sandbox.</p>
      </Dialog>
    </PreviewShell>
  )
}
