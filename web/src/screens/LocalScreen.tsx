import { useEffect, useState } from 'react'
import { CheckResult } from '../components/cover'
import { Play, RefreshCw, Square } from '../components/icons'
import { api, asServer, local, users, type CoverCheck, type LocalProxy, type User } from '../lib/api'
import { DOMAIN, crashCause, dropLine, formatDuration, openConnections, relativeTime, restartDone, validUserName } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate, type Tab } from '../hooks/useRoute'
import { Badge, Button, Card, Confirm, CopyButton, Dialog, ErrorBox, Field, Impact, Input, QrCode, Spinner, Tabs, useToast } from '../components/ui'
import { AttentionList, sentence, type AttentionItem } from './Attention'
import { LogsTab } from './LogsTab'
import { CoverDialog } from './SettingsTab'
import { UsersTab } from './UsersTab'
import { LocalSettings } from './LocalSettings'
import { LocalStatus, proxyState } from './LocalStatus'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

const TABS: [Tab, string][] = [
  ['users', 'Users'],
  ['status', 'Status'],
  ['settings', 'Settings'],
  ['logs', 'Logs'],
]

/** The first run: a cover site, checked from the router, and the first user. */
function Setup({ proxy, onDone }: { proxy: LocalProxy; onDone: (u: User | null) => void }) {
  const toast = useToast()
  const [domain, setDomain] = useState('')
  const [name, setName] = useState('')
  const [check, setCheck] = useState<{ domain: string; result: CoverCheck } | null>(null)
  const [busy, setBusy] = useState<'check' | 'save' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const wanted = domain.trim().toLowerCase()
  const result = check?.domain === wanted ? check.result : null
  const nameOk = validUserName(name.trim())
  const a = proxy.address

  const runCheck = async () => {
    setBusy('check')
    setError(null)
    setCheck(null)
    try {
      setCheck({ domain: wanted, result: await api.checkCover(1, wanted) })
    } catch (e) {
      setError(message(e))
    } finally {
      setBusy(null)
    }
  }

  const finish = async () => {
    setBusy('save')
    setError(null)
    try {
      await local.setup(wanted, name.trim())
      const first = (await users.list(1)).find((u) => u.name === name.trim()) ?? null
      toast('Local MTProto is set up')
      onDone(first)
    } catch (e) {
      setError(message(e))
      setBusy(null)
    }
  }

  return (
    <div className="mx-auto flex max-w-2xl flex-col gap-4">
      <div>
        <h1 className="text-lg font-semibold">Set up Local MTProto</h1>
        <p className="text-[13px] text-muted">People reach this proxy from anywhere through the router's domestic link. It takes two steps.</p>
      </div>
      {a.reported && !a.domestic && (
        <ErrorBox>This router has no domestic link, so the proxy can't take connections. Add one with the panel's setup wizard first.</ErrorBox>
      )}
      <Card className="flex flex-col gap-3 p-4">
        <h2 className="font-semibold">1. Cover site</h2>
        <Field
          label="Cover site"
          hint={wanted && !DOMAIN.test(wanted) ? 'Give a domain name, like www.example.org.' : 'Anyone probing the proxy sees this site. Pick a popular site that loads well from here.'}
        >
          {(p) => (
            <div className="flex gap-2">
              <Input
                {...p}
                value={domain}
                onChange={(e) => setDomain(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && DOMAIN.test(wanted) && void runCheck()}
                spellCheck={false}
                autoCapitalize="off"
                placeholder="www.example.org"
              />
              <Button busy={busy === 'check'} disabled={!DOMAIN.test(wanted) || !!busy} onClick={runCheck}>
                {result ? 'Check again' : 'Check'}
              </Button>
            </div>
          )}
        </Field>
        {result && <CheckResult check={result} domain={wanted} />}
      </Card>
      <Card className="flex flex-col gap-3 p-4">
        <h2 className="font-semibold">2. First user</h2>
        <Field label="Name" hint={name.trim() && !nameOk ? 'Use 1 to 32 letters, digits, dots, dashes or underscores.' : 'Who the first link is for, like mom or my-phone.'}>
          {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} autoCapitalize="off" spellCheck={false} placeholder="mom" />}
        </Field>
      </Card>
      {error && <ErrorBox>{error}</ErrorBox>}
      <div className="flex justify-end">
        <Button variant="primary" busy={busy === 'save'} disabled={!result?.ok || !nameOk || !!busy} onClick={finish} className="w-full sm:w-auto">
          Finish setup
        </Button>
      </div>
    </div>
  )
}

/** Right after setup: the first link. */
function FirstLink({ user, proxy, onClose }: { user: User; proxy: LocalProxy; onClose: () => void }) {
  const ready = !!proxy.address.host && proxy.address.port > 0
  return (
    <Dialog onClose={onClose} title={`${user.name}'s link is ready`} footer={<Button variant="primary" onClick={onClose}>Done</Button>}>
      <div className="flex flex-col items-center gap-4">
        {ready ? (
          <>
            <QrCode text={user.links.https} size={200} />
            <CopyButton text={user.links.https} label="Copy link" variant="primary" size="md" className="w-full" />
          </>
        ) : (
          <p className="text-[13px] text-muted">The link needs the router's address first. Turn on IP Cloud in the router, or type an address under Settings.</p>
        )}
      </div>
    </Dialog>
  )
}

/** What needs looking at, worst first, with a fix where there is one. */
function attention(p: LocalProxy, tab: Tab, cause: { text: string; at: string | null } | null, acts: { restart: () => void; start: () => void; cover: () => void }) {
  const out: AttentionItem[] = []
  const t = p.proxy
  const a = p.address
  const open = (to: Tab) => navigate({ page: 'local', tab: to })
  if (!a.reported)
    out.push(
      p.uptimeSec > 180
        ? { tone: 'warn', title: "The router hasn't reported yet", detail: 'Its script runs every minute and sets up the port. If this stays, reinstall the plugin from the store.' }
        : { tone: 'info', title: 'Waiting for the router', detail: 'Its script sets up the port and reports within a minute.' },
    )
  else if (!a.domestic)
    out.push({
      tone: 'danger',
      title: 'This router has no domestic link',
      detail: "Phones reach the proxy through the domestic link, so the proxy stays off. Add one with the panel's setup wizard.",
    })
  if (t.crashLoop)
    out.push({
      tone: 'danger',
      title: 'The proxy keeps crashing',
      detail: cause ? `${cause.text} From the log${cause.at ? `, ${relativeTime(cause.at)}` : ''}.` : `${t.lastExit ? `It last stopped with "${t.lastExit}". ` : ''}The log may say why.`,
      acts: [{ label: 'Restart proxy', primary: true, run: acts.restart }, ...(tab === 'logs' ? [] : [{ label: 'View logs', run: () => open('logs') }])],
    })
  if (p.proxyStopped)
    out.push({ tone: 'warn', title: 'You stopped the proxy', detail: 'It stays off, across restarts, until you start it.', acts: [{ label: 'Start proxy', primary: true, run: acts.start }] })
  else if (!t.running && !t.crashLoop && a.domestic && t.held)
    out.push(
      p.userCount === 0
        ? { tone: 'info', title: 'The proxy starts with its first user', detail: 'Add a user to get a link.' }
        : { tone: 'warn', title: 'The proxy is not running', detail: sentence(t.held) },
    )
  if (a.reported && a.domestic && (!a.host || !a.port))
    out.push({
      tone: 'warn',
      title: 'Links have no address yet',
      detail: `IP Cloud has no name for this router${a.cloudStatus ? ` (${a.cloudStatus})` : ''}. Turn IP Cloud on in the router, or type an address under Settings.`,
      acts: tab === 'settings' ? [] : [{ label: 'Open Settings', run: () => open('settings') }],
    })
  if (p.cover.checkedAt && !p.cover.ok)
    out.push({
      tone: 'warn',
      title: "The cover site isn't working",
      detail: `${p.cover.domain}: ${p.cover.error || 'the check failed'}. Anyone probing the proxy would notice.`,
      acts: [{ label: 'Change cover site…', run: acts.cover }],
    })
  if (a.newPortAskedAt) {
    const late = Date.now() - Date.parse(a.newPortAskedAt) > 3 * 60e3
    out.push(
      late
        ? { tone: 'warn', title: "The router hasn't moved the port", detail: 'Its script runs every minute. If it stays like this, reinstall the plugin from the store.' }
        : { tone: 'info', title: 'Moving to a new port', detail: 'The router moves the forward within a minute. Links change once it has.' },
    )
  }
  if (t.applyError) out.push({ tone: 'warn', title: "Settings aren't applied", detail: sentence(t.applyError) })
  return out
}

export function LocalScreen({ tab }: { tab: Tab }) {
  const toast = useToast()
  const [restart, setRestart] = useState<{ at: number; before?: string } | null>(null)
  const detail = usePoll(local.get, restart ? 2000 : 5000)
  const [modal, setModal] = useState<'restart' | 'stop' | 'cover' | null>(null)
  const [starting, setStarting] = useState(false)
  const [first, setFirst] = useState<User | null>(null)
  const p = detail.data
  const crash = !!p?.proxy.crashLoop
  const crashLogs = usePoll(() => (crash ? api.logs(1, 100) : Promise.resolve(null)), 30000, [crash])
  const cause = crashLogs.data ? crashCause(crashLogs.data.lines) : null
  const server = p ? asServer(p) : null

  // "Restarting…" lasts until the proxy is back, or a minute
  useEffect(() => {
    if (restart && server && restartDone(server, restart.before, Date.now() - restart.at)) {
      setRestart(null)
      toast('Proxy restarted')
    }
  }, [server, restart, toast])
  useEffect(() => {
    if (!restart) return
    const t = setTimeout(() => {
      setRestart(null)
      toast("The proxy isn't back after a minute. The log may say why.", 'error', {
        label: 'View logs',
        run: () => navigate({ page: 'local', tab: 'logs' }),
      })
    }, 60000)
    return () => clearTimeout(t)
  }, [restart, toast])

  if (!p || !server) return detail.error ? <ErrorBox>{detail.error}</ErrorBox> : <Spinner />
  const reload = () => void detail.reload()
  if (!p.coverDomain)
    return (
      <Setup
        proxy={p}
        onDone={(u) => {
          setFirst(u)
          reload()
        }}
      />
    )

  const startProxy = async () => {
    setStarting(true)
    try {
      await api.stopProxy(1, false)
      toast('Proxy started')
      await detail.reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setStarting(false)
    }
  }
  const state = proxyState(p)
  const a = p.address
  const n = openConnections(server)
  const close = () => setModal(null)
  const canRestart = p.proxy.running || p.proxy.crashLoop

  return (
    <div className="flex flex-col gap-5">
      {first && <FirstLink user={first} proxy={p} onClose={() => setFirst(null)} />}
      <Card className="flex flex-col gap-4 p-4 sm:flex-row sm:items-center">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-lg font-semibold">This router</h1>
            <Badge tone={state.tone}>proxy {state.text}</Badge>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
            <span className="font-mono">{a.host && a.port ? `${a.host}:${a.port}` : 'no address for links yet'}</span>
            <span>cover {p.coverDomain}</span>
            {p.proxy.version && <span>proxy {p.proxy.version}</span>}
            {p.proxy.running && <span>up {formatDuration(p.proxy.uptimeSec)}</span>}
          </div>
        </div>
        <div role="group" aria-label="Proxy actions" className="grid grid-cols-2 gap-2 sm:flex">
          <Button
            size="sm"
            busy={!!restart}
            disabled={p.proxyStopped || !canRestart}
            title={p.proxyStopped ? 'Start the proxy first' : !canRestart ? "It isn't running" : undefined}
            onClick={() => setModal('restart')}
          >
            {!restart && <RefreshCw className="h-3.5 w-3.5" />} {restart ? 'Restarting…' : 'Restart proxy'}
          </Button>
          {p.proxyStopped ? (
            <Button size="sm" variant="primary" busy={starting} onClick={startProxy}>
              <Play className="h-3.5 w-3.5" /> Start proxy
            </Button>
          ) : (
            <Button size="sm" onClick={() => setModal('stop')}>
              <Square className="h-3.5 w-3.5" /> Stop proxy
            </Button>
          )}
        </div>
      </Card>

      {detail.error && <ErrorBox>{detail.error}</ErrorBox>}
      <AttentionList list={attention(p, tab, cause, { restart: () => setModal('restart'), start: () => void startProxy(), cover: () => setModal('cover') })} />

      <Tabs label="Proxy sections" idBase="srv" tabs={TABS} value={tab} onChange={(t) => navigate({ page: 'local', tab: t })} />
      <div role="tabpanel" id="srv-panel" aria-labelledby={`srv-${tab}`}>
        {tab === 'users' && <UsersTab server={server} />}
        {tab === 'status' && <LocalStatus proxy={p} />}
        {tab === 'settings' && <LocalSettings proxy={p} onChanged={reload} onChangeCover={() => setModal('cover')} />}
        {tab === 'logs' && <LogsTab server={server} />}
      </div>

      {modal === 'restart' && (
        <Confirm
          title="Restart the proxy?"
          action="Restart proxy"
          onClose={close}
          onConfirm={async () => {
            await api.restart(1)
            setRestart({ at: Date.now(), before: server.agent?.reportedAt })
          }}
        >
          <Impact n={n}>{dropLine(server)}</Impact>
          <p>The button shows "Restarting…" until the proxy is back. If it isn't back within a minute, you'll see why.</p>
        </Confirm>
      )}
      {modal === 'stop' && (
        <Confirm
          danger
          title="Stop the proxy?"
          action="Stop proxy"
          onClose={close}
          onConfirm={async () => {
            await api.stopProxy(1, true)
            toast('Proxy stopped')
            reload()
          }}
        >
          <Impact n={n}>{dropLine(server).replace(/ Telegram reconnects.*$/, '')}</Impact>
          <p>It stays off, across restarts, until you press Start proxy.</p>
        </Confirm>
      )}
      {modal === 'cover' && <CoverDialog server={server} onClose={close} onChanged={reload} />}
    </div>
  )
}
