import { useState, type ReactNode } from 'react'
import { CheckResult } from '../components/cover'
import { RefreshCw } from '../components/icons'
import { api, type Allowlist, type AppConfig, type CoverCheck, type Server } from '../lib/api'
import { DOMAIN, plural, relativeTime, streamsLeft } from '../lib/format'
import { Badge, Button, Card, ErrorBox, Field, Input, Notice, useToast } from '../components/ui'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 py-2.5 sm:grid-cols-[180px_1fr] sm:gap-4">
      <dt className="text-[13px] text-muted">{label}</dt>
      <dd className="min-w-0 break-all text-[13px]">{children}</dd>
    </div>
  )
}

function CoverCard({ server, onChanged }: { server: Server; onChanged: () => void }) {
  const toast = useToast()
  const [domain, setDomain] = useState('')
  const [check, setCheck] = useState<{ domain: string; result: CoverCheck } | null>(null)
  const [busy, setBusy] = useState<'check' | 'save' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const wanted = domain.trim().toLowerCase()
  const valid = DOMAIN.test(wanted) && wanted !== server.coverDomain
  const passed = check?.domain === wanted && check.result.ok
  const last = server.agent?.cover

  const runCheck = async () => {
    setBusy('check')
    setError(null)
    setCheck(null)
    try {
      setCheck({ domain: wanted, result: await api.checkCover(server.id, wanted) })
    } catch (e) {
      setError(message(e))
    } finally {
      setBusy(null)
    }
  }

  const save = async () => {
    setBusy('save')
    setError(null)
    try {
      await api.settings(server.id, { coverDomain: wanted })
      toast(`Cover site is now ${wanted}; share the new links`)
      setDomain('')
      setCheck(null)
      onChanged()
    } catch (e) {
      setError(message(e))
    } finally {
      setBusy(null)
    }
  }

  return (
    <Card className="flex flex-col gap-3 p-4">
      <div>
        <h2 className="font-semibold">Cover site</h2>
        <p className="text-[13px] text-muted">The proxy and its link look like visits to this site.</p>
      </div>
      <div className="flex flex-wrap items-center gap-2 text-[13px]">
        <b>{server.coverDomain}</b>
        {last &&
          (!last.checkedAt ? (
            <Badge tone="idle">not checked yet</Badge>
          ) : last.ok ? (
            <Badge tone="online">working</Badge>
          ) : (
            <Badge tone="danger">failing</Badge>
          ))}
        {last?.checkedAt && (
          <span className="text-xs text-faint">checked {relativeTime(last.checkedAt)}</span>
        )}
      </div>
      {last?.checkedAt && !last.ok && last.error && <ErrorBox>{last.error}</ErrorBox>}
      <Notice tone="warn">Changing it changes every user's link: the old links stop working.</Notice>
      <div className="flex flex-wrap items-end gap-2">
        <Field label="New cover site" className="min-w-[200px] flex-1">
          {(id) => (
            <Input
              id={id}
              value={domain}
              onChange={(e) => setDomain(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && valid && void runCheck()}
              spellCheck={false}
              autoCapitalize="off"
            />
          )}
        </Field>
        <Button busy={busy === 'check'} disabled={!valid || !!busy} onClick={runCheck}>
          Check
        </Button>
        <Button variant="primary" busy={busy === 'save'} disabled={!passed || !!busy} onClick={save}>
          Use this site
        </Button>
      </div>
      <p className="text-xs text-faint">The server checks the site before it is used.</p>
      {check?.domain === wanted && <CheckResult check={check.result} />}
      {error && <ErrorBox>{error}</ErrorBox>}
    </Card>
  )
}

function StreamsCard({
  server,
  config,
  onChanged,
}: {
  server: Server
  config: AppConfig | null
  onChanged: () => void
}) {
  const toast = useToast()
  const [value, setValue] = useState(String(server.maxStreams))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // what this server may take: its own share plus what nobody holds
  const most = config ? server.maxStreams + streamsLeft(config) : null

  const save = async () => {
    const n = Number(value)
    if (!Number.isInteger(n) || n < 100) return setError('Use a whole number of at least 100.')
    setBusy(true)
    setError(null)
    try {
      const saved = await api.settings(server.id, { maxStreams: n })
      toast(`Stream limit set to ${saved.maxStreams}`)
      onChanged()
    } catch (e) {
      setError(message(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="flex flex-col gap-3 p-4">
      <div>
        <h2 className="font-semibold">Streams</h2>
        <p className="text-[13px] text-muted">Telegram connections this server may hold at once.</p>
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <Field label="Stream limit" className="min-w-[200px] flex-1">
          {(id) => (
            <Input
              id={id}
              type="number"
              min={100}
              step={100}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && void save()}
            />
          )}
        </Field>
        <Button variant="primary" busy={busy} disabled={value === String(server.maxStreams)} onClick={save}>
          Save limit
        </Button>
      </div>
      {most !== null && (
        <p className="text-xs text-faint">
          Up to {most} on this server; the router has {config!.streamBudget} in total.
        </p>
      )}
      <Notice tone="warn">Saving reconnects the server: users' open connections drop.</Notice>
      {error && <ErrorBox>{error}</ErrorBox>}
    </Card>
  )
}

function AllowlistCard({ config, onChanged }: { config: AppConfig; onChanged: () => void }) {
  const toast = useToast()
  const [fresh, setFresh] = useState<Allowlist | null>(null)
  const [busy, setBusy] = useState(false)
  const list = fresh ?? config.allowlist
  const count = plural(list.prefixes, 'prefix', 'prefixes')

  const refresh = async () => {
    setBusy(true)
    try {
      const next = await api.refreshAllowlist()
      setFresh(next)
      if (next.lastError) toast(`Refresh failed: ${next.lastError}`, 'error')
      else toast(`Allowlist refreshed: ${plural(next.prefixes, 'prefix', 'prefixes')}`)
      onChanged()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <Card className="flex flex-col gap-3 p-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div>
            <h2 className="font-semibold">Telegram allowlist</h2>
            <p className="text-[13px] text-muted">
              The router dials only these addresses, for every server.
            </p>
          </div>
          <Button size="sm" busy={busy} disabled={!list.url} onClick={refresh}>
            <RefreshCw className="h-3.5 w-3.5" /> Refresh now
          </Button>
        </div>
        {list.placeholder && (
          <Notice tone="warn">
            The built-in list is still a placeholder, so the router doesn't carry real Telegram traffic yet.
          </Notice>
        )}
        <dl className="divide-y divide-border">
          <Row label="Size">{count}</Row>
          <Row label="Last refresh">{list.refreshedAt ? relativeTime(list.refreshedAt) : 'never'}</Row>
          <Row label="Source">
            {list.url ? <span className="font-mono text-xs">{list.url}</span> : 'built-in list only'}
          </Row>
        </dl>
        {list.lastError && <ErrorBox>{list.lastError}</ErrorBox>}
      </Card>
    </section>
  )
}

export function SettingsTab({
  server,
  config,
  onChanged,
}: {
  server: Server
  config: AppConfig | null
  onChanged: () => void
}) {
  return (
    <div className="flex flex-col gap-4">
      <CoverCard server={server} onChanged={onChanged} />
      <StreamsCard key={server.maxStreams} server={server} config={config} onChanged={onChanged} />
      {config && <AllowlistCard config={config} onChanged={onChanged} />}
      <Card className="p-4">
        <h2 className="font-semibold">Server</h2>
        <dl className="mt-2 divide-y divide-border">
          <Row label="Address">
            <span className="font-mono">{server.host}</span>
          </Row>
          <Row label="SSH">
            <span className="font-mono">
              {server.sshUser}@{server.host}:{server.sshPort}
            </span>
          </Row>
          <Row label="Host key">
            <span className="font-mono text-xs">{server.hostKey}</span>
          </Row>
          <Row label="Link port">{server.hubPort}</Row>
          <Row label="Architecture">{server.arch || '–'}</Row>
          <Row label="Agent version">{server.agentVersion || '–'}</Row>
          <Row label="Proxy version">{server.telemtVersion || '–'}</Row>
          <Row label="Added">{new Date(server.createdAt).toLocaleString()}</Row>
        </dl>
      </Card>
    </div>
  )
}
