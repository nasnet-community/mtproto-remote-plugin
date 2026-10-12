import { useEffect, useState, type ReactNode } from 'react'
import { CheckResult } from '../components/cover'
import { Pencil, RefreshCw, TriangleAlert, X } from '../components/icons'
import { ApiError, api, type Allowlist, type AppConfig, type CoverCheck, type LogLevel, type Server } from '../lib/api'
import { DOMAIN, dropLine, formatCount, openConnections, plural, relativeTime, streamsLeft } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { setLeaveGuard } from '../hooks/useRoute'
import { Badge, Button, Card, Confirm, Dialog, Disclosure, ErrorBox, Field, Impact, Input, Notice, Select, cx, useToast } from '../components/ui'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))
const NEEDS_LINK = 'Needs the link to this server'

export function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 py-2.5 sm:grid-cols-[180px_1fr] sm:gap-4">
      <dt className="text-[13px] text-muted">{label}</dt>
      <dd className="min-w-0 break-all text-[13px]">{children}</dd>
    </div>
  )
}

export function Head({ title, hint, children }: { title: string; hint: string; children?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <h2 className="font-semibold">{title}</h2>
        <p className="text-[13px] text-muted">{hint}</p>
      </div>
      {children}
    </div>
  )
}

/** Change the cover site in steps: type it, check it from the server, then confirm every link changes. */
export function CoverDialog({ server, onClose, onChanged }: { server: Server; onClose: () => void; onChanged: () => void }) {
  const toast = useToast()
  const [domain, setDomain] = useState('')
  const [check, setCheck] = useState<{ domain: string; result: CoverCheck } | null>(null)
  const [busy, setBusy] = useState<'check' | 'save' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const wanted = domain.trim().toLowerCase()
  const valid = DOMAIN.test(wanted) && wanted !== server.coverDomain
  const result = check?.domain === wanted ? check.result : null

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
      onChanged()
      onClose()
    } catch (e) {
      setError(message(e))
      setBusy(null)
    }
  }

  return (
    <Dialog
      onClose={onClose}
      title="Change cover site"
      description={`${server.name} uses ${server.coverDomain} now.`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={busy === 'save'} disabled={!result?.ok || !!busy} onClick={save} className="min-w-0">
            {/* a long domain won't fit beside Cancel on a phone */}
            <span className="truncate sm:hidden">Use this site</span>
            <span className="hidden truncate sm:inline">{result?.ok ? `Use ${wanted}` : 'Use this site'}</span>
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Field
          label="New cover site"
          hint={
            wanted && !valid
              ? wanted === server.coverDomain
                ? 'That is the cover site now.'
                : 'Give a domain name, like www.example.org.'
              : __LOCAL__
                ? 'The router checks it over the domestic link before it is used.'
                : 'The server checks it before it is used.'
          }
        >
          {(p) => (
            <div className="flex gap-2">
              <Input
                {...p}
                value={domain}
                onChange={(e) => setDomain(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && valid && void runCheck()}
                spellCheck={false}
                autoCapitalize="off"
                placeholder="www.example.org"
              />
              <Button busy={busy === 'check'} disabled={!valid || !!busy} onClick={runCheck}>
                {result ? 'Check again' : 'Check'}
              </Button>
            </div>
          )}
        </Field>
        {result && <CheckResult check={result} domain={wanted} />}
        {result?.ok && (
          <div className="flex gap-3 rounded-md bg-warn-tint p-3 text-[13px]">
            <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-warn-text" />
            <span>
              <b>All {plural(server.userCount, 'user')} get new links.</b> Their old links stop working, so you'll need to share the new ones.
            </span>
          </div>
        )}
        {error && <ErrorBox>{error}</ErrorBox>}
      </div>
    </Dialog>
  )
}

/** Move the link to another port; the job moves it back if the link doesn't come up there. */
export function LinkPortDialog({ server, onClose, onStart }: { server: Server; onClose: () => void; onStart: (port: number) => Promise<void> }) {
  const [port, setPort] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const n = Number(port)
  const problem = !port.trim()
    ? null
    : !Number.isInteger(n) || n < 1024 || n > 65535
      ? 'Use a port from 1024 to 65535.'
      : n === server.hubPort
        ? 'That is the port it uses now.'
        : n === server.sshPort || n === 22
          ? "That is the server's SSH port."
          : null
  const start = async () => {
    setBusy(true)
    setError(null)
    try {
      await onStart(n)
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }
  return (
    <Dialog
      onClose={onClose}
      title="Change the link port"
      description={`${server.name} links on port ${server.hubPort} now.`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="link-port" busy={busy} disabled={!port.trim() || !!problem}>
            Change port
          </Button>
        </>
      }
    >
      <form
        id="link-port"
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (port.trim() && !problem) void start()
        }}
      >
        <Field
          label="New link port"
          error={problem}
          hint={`Open it on the server's firewall first. If the link doesn't come up on it, it moves back to ${server.hubPort}.`}
        >
          {(p) => <Input {...p} inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} placeholder="9000" />}
        </Field>
        {error && <ErrorBox>{error}</ErrorBox>}
      </form>
    </Dialog>
  )
}

export function CoverCard({ server, onChange }: { server: Server; onChange: () => void }) {
  const last = server.agent?.cover
  const down = server.link.state === 'down'
  return (
    <Card className="flex flex-col gap-2 p-4">
      <Head title="Cover site" hint={__LOCAL__ ? 'Anyone probing the proxy sees this site.' : 'The proxy and its link look like visits to this site.'}>
        <Button size="sm" disabled={down} title={down ? NEEDS_LINK : undefined} onClick={onChange}>
          <Pencil className="h-3.5 w-3.5" /> Change cover site…
        </Button>
      </Head>
      <div className="flex flex-wrap items-center gap-2 text-[13px]">
        <b>{server.coverDomain}</b>
        {last &&
          (!last.checkedAt ? <Badge tone="idle">not checked yet</Badge> : last.ok ? <Badge tone="online">working</Badge> : <Badge tone="danger">failing</Badge>)}
        {last?.checkedAt && <span className="text-xs text-faint">checked {relativeTime(last.checkedAt)}</span>}
      </div>
      {last?.checkedAt && !last.ok && last.error && <ErrorBox>{last.error}</ErrorBox>}
      {down && <p className="text-xs text-faint">{NEEDS_LINK} to change it.</p>}
    </Card>
  )
}

const SHADES = ['bg-faint', 'bg-border-strong', 'bg-muted']

function ConnectionsCard({ server, config, onChanged }: { server: Server; config: AppConfig | null; onChanged: () => void }) {
  const toast = useToast()
  const servers = usePoll(api.servers, 30000)
  const [value, setValue] = useState(server.maxStreams)
  const [asking, setAsking] = useState(false)
  const dirty = value !== server.maxStreams
  const free = config ? Math.max(0, streamsLeft(config)) : 0
  // this server's share plus what nobody holds, at most 5,000
  const most = Math.max(server.maxStreams, Math.min(5000, server.maxStreams + free))

  useEffect(() => {
    setLeaveGuard('connections', dirty ? `The connection limit (${formatCount(server.maxStreams)} → ${formatCount(value)}) isn't saved.` : null)
    return () => setLeaveGuard('connections', null)
  }, [dirty, value, server.maxStreams])

  const budget = config?.streamBudget ?? 0
  const others = servers.data
    ? servers.data.filter((s) => s.id !== server.id).map((s) => ({ name: s.name, n: s.maxStreams }))
    : config
      ? [{ name: 'Other servers', n: config.streamsAllocated - server.maxStreams }]
      : []
  const used = value + others.reduce((t, o) => t + o.n, 0)
  const pct = (n: number) => `${budget ? (n / budget) * 100 : 0}%`

  return (
    <Card className="flex flex-col gap-3 p-4">
      <Head title="Connections" hint="How many Telegram connections this server can hold at once.">
        {dirty && (
          <span className="rounded-full bg-warn-tint px-2.5 py-0.5 text-xs font-medium text-warn-text">
            Unsaved: {formatCount(server.maxStreams)} → {formatCount(value)}
          </span>
        )}
      </Head>
      <Field
        label={
          <span className="flex justify-between text-foreground">
            Connection limit <b className="tabular">{formatCount(value)}</b>
          </span>
        }
      >
        {(p) => (
          <div>
            <input
              {...p}
              type="range"
              min={100}
              max={most}
              step={100}
              value={value}
              onChange={(e) => setValue(Number(e.target.value))}
              className="h-11 w-full sm:h-6"
            />
            <div className="flex justify-between text-xs text-faint">
              <span>100</span>
              <span>{formatCount(most)} (what this router has free for it)</span>
            </div>
          </div>
        )}
      </Field>
      {budget > 0 && (
        <div className="flex flex-col gap-2">
          <div className="flex h-2 gap-0.5 overflow-hidden rounded-full bg-track" aria-hidden="true">
            <span className="rounded-full bg-accent" style={{ width: pct(value) }} />
            {others.map((o, i) => (
              <span key={o.name} className={cx('rounded-full', SHADES[i % 3])} style={{ width: pct(o.n) }} />
            ))}
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
            <span className="inline-flex items-center gap-1.5">
              <span className="h-2 w-2 rounded-sm bg-accent" /> This server {formatCount(value)}
            </span>
            {others.map((o, i) => (
              <span key={o.name} className="inline-flex items-center gap-1.5">
                <span className={cx('h-2 w-2 rounded-sm', SHADES[i % 3])} /> {o.name} {formatCount(o.n)}
              </span>
            ))}
            <span>
              Free {formatCount(Math.max(0, budget - used))} of {formatCount(budget)}
            </span>
          </div>
        </div>
      )}
      <div className="flex flex-col gap-2 border-t border-border pt-3 sm:flex-row sm:items-center sm:justify-between">
        <span className="text-xs text-muted">Saving reconnects the server. You'll see how many connections drop before it does.</span>
        <div className="grid grid-flow-col auto-cols-fr gap-2 sm:flex">
          {dirty && (
            <Button size="sm" onClick={() => setValue(server.maxStreams)}>
              Discard
            </Button>
          )}
          <Button size="sm" variant="primary" disabled={!dirty} onClick={() => setAsking(true)}>
            Save…
          </Button>
        </div>
      </div>
      {asking && (
        <Confirm
          title="Save the connection limit?"
          action="Save and reconnect"
          onClose={() => setAsking(false)}
          onConfirm={async () => {
            const saved = await api.settings(server.id, { maxStreams: value })
            toast(`Connection limit set to ${formatCount(saved.maxStreams)}`)
            onChanged()
          }}
        >
          <Impact n={openConnections(server)}>{dropLine(server)}</Impact>
          <p>
            {server.name} can then hold {formatCount(value)} connections at once.
          </p>
        </Confirm>
      )}
    </Card>
  )
}

function LinkCard({ server, onChange }: { server: Server; onChange: () => void }) {
  const down = server.link.state === 'down'
  return (
    <Card className="flex flex-col gap-2 p-4">
      <Head title="Link" hint="How this router reaches the server's agent.">
        <Button size="sm" disabled={down} title={down ? NEEDS_LINK : undefined} onClick={onChange}>
          Change link port…
        </Button>
      </Head>
      <div className="text-[13px]">
        Link port <b>{server.hubPort}</b>
        <span className="text-muted">
          {' '}
          · {server.link.up} of {server.link.total} connections up
        </span>
      </div>
    </Card>
  )
}

const LEVELS: [LogLevel, string][] = [
  ['silent', 'Quiet'],
  ['normal', 'Normal'],
  ['verbose', 'Verbose'],
  ['debug', 'Debug'],
]

export function AdvancedCard({ server, onChanged }: { server: Server; onChanged: () => void }) {
  const toast = useToast()
  const o = server.proxyOptions
  const [names, setNames] = useState(o.extraCoverNames)
  const [win, setWin] = useState(String(o.deviceWindowSecs))
  const [level, setLevel] = useState<LogLevel>(o.logLevel)
  const [adding, setAdding] = useState('')
  const [check, setCheck] = useState<{ domain: string; result: CoverCheck } | null>(null)
  const [busy, setBusy] = useState<'check' | 'save' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [asking, setAsking] = useState(false)
  const down = server.link.state === 'down'
  const secs = Number(win)
  const badWin = !Number.isInteger(secs) || secs < 0 || secs > 86400
  // these two restart the proxy; the device window does not
  const core = names.join() !== o.extraCoverNames.join() || level !== o.logLevel
  const dirty = core || secs !== o.deviceWindowSecs

  useEffect(() => {
    setLeaveGuard('options', dirty ? "The advanced proxy options aren't saved." : null)
    return () => setLeaveGuard('options', null)
  }, [dirty])

  const wanted = adding.trim().toLowerCase()
  const addProblem = !wanted
    ? null
    : !DOMAIN.test(wanted)
      ? 'Give a domain name, like cdn.example.net.'
      : wanted === server.coverDomain
        ? 'That is the main cover site.'
        : names.includes(wanted)
          ? 'That name is in the list already.'
          : null

  const add = async () => {
    setBusy('check')
    setError(null)
    setCheck(null)
    try {
      const result = await api.checkCover(server.id, wanted)
      if (result.ok) {
        setNames((n) => [...n, wanted])
        setAdding('')
      } else setCheck({ domain: wanted, result })
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
      await api.settings(server.id, { proxyOptions: { extraCoverNames: names, deviceWindowSecs: secs, logLevel: level } })
      toast('Proxy options saved')
      onChanged()
    } catch (e) {
      setError(message(e))
      throw e
    } finally {
      setBusy(null)
    }
  }

  return (
    <Card className="p-4">
      <Disclosure title="Advanced proxy options" hint={`For tuning. The defaults suit most ${__LOCAL__ ? 'uses' : 'servers'}.`}>
        <div className="flex flex-col gap-4 border-t border-border pt-4">
          <div className="flex flex-col gap-1.5">
            <span className="text-[13px] font-medium text-muted">Extra cover names</span>
            <div className="flex flex-wrap items-center gap-2">
              {names.map((n) => (
                <span key={n} className="inline-flex h-11 items-center gap-1 rounded-full border border-border bg-card2 pl-3 text-[13px] sm:h-8">
                  {n}
                  <button
                    type="button"
                    aria-label={`Remove ${n}`}
                    onClick={() => setNames((l) => l.filter((x) => x !== n))}
                    className="flex h-11 w-9 items-center justify-center rounded-full text-muted hover:text-foreground sm:h-8 sm:w-7"
                  >
                    <X className="h-3.5 w-3.5" />
                  </button>
                </span>
              ))}
              {names.length < 4 && (
                <div className="flex min-w-[240px] flex-1 gap-2">
                  <Input
                    aria-label="Add a cover name"
                    value={adding}
                    onChange={(e) => setAdding(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter') return
                      e.preventDefault()
                      if (wanted && !addProblem && !down) void add()
                    }}
                    spellCheck={false}
                    autoCapitalize="off"
                    placeholder="Add a site, like cdn.example.net"
                  />
                  <Button busy={busy === 'check'} disabled={!wanted || !!addProblem || down || !!busy} title={down ? NEEDS_LINK : undefined} onClick={add}>
                    Check and add
                  </Button>
                </div>
              )}
            </div>
            {addProblem ? (
              <p className="text-xs text-bad-text">{addProblem}</p>
            ) : (
              <p className="text-xs text-faint">
                Each name gives every user one more link to fall back on if a name gets blocked. Users' Share sheets list them.
                {down && ` Checking a name needs the link to this server.`}
              </p>
            )}
            {check?.domain === wanted && <CheckResult check={check.result} domain={wanted} />}
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label="Device window"
              error={badWin && 'Use 0 to 86,400 seconds.'}
              hint="How long an address keeps counting toward a user's device limit after it disconnects. 0 uses the proxy's default."
            >
              {(p) => (
                <div className="flex items-center gap-2">
                  <Input {...p} inputMode="numeric" value={win} onChange={(e) => setWin(e.target.value)} className="max-w-[8rem]" />
                  <span className="text-[13px] text-muted">seconds</span>
                </div>
              )}
            </Field>
            <Field label="Proxy log level" hint="Raise it to look into connections that fail. Lower it again afterwards.">
              {(p) => (
                <Select {...p} value={level} onChange={(e) => setLevel(e.target.value as LogLevel)} className="sm:w-48">
                  {LEVELS.map(([v, label]) => (
                    <option key={v} value={v}>
                      {label}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
          </div>
          {error && <ErrorBox>{error}</ErrorBox>}
          <div className="flex flex-col gap-2 border-t border-border pt-3 sm:flex-row sm:items-center sm:justify-between">
            <span className="text-xs text-muted">Some of these restart the proxy. You'll be asked first.</span>
            <Button
              size="sm"
              variant={dirty ? 'primary' : 'secondary'}
              busy={busy === 'save'}
              disabled={!dirty || badWin || !!busy}
              onClick={() => (core ? setAsking(true) : void save().catch(() => {}))}
            >
              Save options
            </Button>
          </div>
        </div>
      </Disclosure>
      {asking && (
        <Confirm title="Save and restart the proxy?" action="Save and restart" onClose={() => setAsking(false)} onConfirm={save}>
          <Impact n={openConnections(server)}>{dropLine(server)}</Impact>
          <p>New cover names and the log level take effect when the proxy restarts.</p>
        </Confirm>
      )}
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
      if (next.lastError) toast(`Couldn't refresh the allowlist. ${next.lastError}`, 'error')
      else toast(`Allowlist refreshed: ${plural(next.prefixes, 'prefix', 'prefixes')}`)
      onChanged()
    } catch (e) {
      // a failed refresh still says how the list stands
      const kept = e instanceof ApiError ? (e.body.allowlist as Allowlist | undefined) : undefined
      if (kept) setFresh(kept)
      const why = (kept?.lastError || message(e)).replace(/\.?$/, '.')
      toast(`Couldn't refresh the allowlist. ${why}${kept ? ` The router keeps the ${plural(kept.prefixes, 'prefix', 'prefixes')} it has.` : ''}`, 'error')
      if (kept) onChanged()
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <Card className="flex flex-col gap-3 p-4">
        <Head title="Telegram allowlist" hint="The router dials only these addresses, for every server.">
          <Button size="sm" busy={busy} disabled={!list.url} onClick={refresh}>
            <RefreshCw className="h-3.5 w-3.5" /> Refresh now
          </Button>
        </Head>
        {list.placeholder && (
          <Notice tone="warn">The built-in list is still a placeholder, so the router doesn't carry real Telegram traffic yet.</Notice>
        )}
        <dl className="divide-y divide-border">
          <Row label="Size">{count}</Row>
          <Row label="Last refresh">{list.refreshedAt ? relativeTime(list.refreshedAt) : 'never'}</Row>
          <Row label="Source">{list.url ? <span className="font-mono text-xs">{list.url}</span> : 'built-in list only'}</Row>
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
  onChangeCover,
  onChangePort,
}: {
  server: Server
  config: AppConfig | null
  onChanged: () => void
  onChangeCover: () => void
  onChangePort: () => void
}) {
  return (
    <div className="flex flex-col gap-4">
      <CoverCard server={server} onChange={onChangeCover} />
      <ConnectionsCard key={server.maxStreams} server={server} config={config} onChanged={onChanged} />
      <LinkCard server={server} onChange={onChangePort} />
      <AdvancedCard server={server} onChanged={onChanged} />
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
          <Row label="Architecture">{server.arch || '–'}</Row>
          <Row label="Agent version">{server.agentVersion || '–'}</Row>
          <Row label="Proxy version">{server.telemtVersion || '–'}</Row>
          <Row label="Added">
            {new Date(server.createdAt).toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' })}
          </Row>
        </dl>
      </Card>
    </div>
  )
}
