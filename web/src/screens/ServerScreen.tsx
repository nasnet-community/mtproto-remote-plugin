import { useEffect, useRef, useState } from 'react'
import { ChevronLeft, Download, Ellipsis, KeyRound, Pencil, Play, RefreshCw, Square, Trash2, Wrench } from '../components/icons'
import { useJobs } from '../components/jobs'
import {
  HostKeyCheck,
  LoginFields,
  emptyLogin,
  keyNeedsTrust,
  keyTrusted,
  loginError,
  toLogin,
  withoutSecrets,
  type LoginForm,
} from '../components/login'
import { ApiError, api, type AppConfig, type Job, type ProbeResult, type Server } from '../lib/api'
import { crashCause, dropLine, formatDuration, jobTitle, linkTone, openConnections, restartDone } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate, parseRoute, type Tab } from '../hooks/useRoute'
import {
  Badge,
  Button,
  Card,
  Confirm,
  Dialog,
  ErrorBox,
  Field,
  Impact,
  Input,
  Menu,
  Notice,
  Spinner,
  Tabs,
  cx,
  useToast,
  type MenuItem,
} from '../components/ui'
import { Attention } from './Attention'
import { UsersTab } from './UsersTab'
import { CoverDialog, LinkPortDialog, SettingsTab } from './SettingsTab'
import { StatusTab } from './StatusTab'
import { LogsTab } from './LogsTab'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

export const NEEDS_LINK = 'Needs the link to this server'

/**
 * Looks at the server's SSH host key before a login goes out: a known or first
 * key goes straight on, a changed one is shown first (a rebuilt VPS has a new one).
 */
function useHostKey(server: Server, login: LoginForm) {
  const [probe, setProbe] = useState<ProbeResult | null>(null)
  const [acked, setAcked] = useState(false)
  return {
    probe,
    acked,
    setAcked,
    /** The key to send, or null while the user has to look at it first. */
    check: async (): Promise<string | null> => {
      const p = await api.probe({ host: server.host, sshPort: Number(login.sshPort), sshUser: login.sshUser.trim() })
      if (!keyNeedsTrust(p)) return p.fingerprint
      setProbe(p)
      setAcked(false)
      return null
    },
    back: () => setProbe(null),
    trusted: probe !== null && keyTrusted(probe, acked),
  }
}

function RepairDialog({ server, onClose, onStarted }: { server: Server; onClose: () => void; onStarted: (jobId: string) => void }) {
  const [login, setLogin] = useState<LoginForm>(() => emptyLogin(server.sshPort, server.sshUser))
  const [reset, setReset] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const key = useHostKey(server, login)
  const repair = async (hostKey?: string) => {
    const problem = loginError(login)
    if (problem) return setError(problem)
    setBusy(true)
    setError(null)
    try {
      const fingerprint = hostKey ?? (await key.check())
      if (fingerprint === null) return setBusy(false)
      const { jobId } = await api.repair(server.id, { ...toLogin(login, fingerprint), reset })
      setLogin(withoutSecrets)
      onStarted(jobId)
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }
  const probe = key.probe
  return (
    <Dialog
      onClose={onClose}
      title={`Repair ${server.name}`}
      description={probe ? undefined : 'Reinstalls the agent over SSH; settings and users stay.'}
      footer={
        probe ? (
          <>
            <Button onClick={key.back}>Back</Button>
            <Button variant="danger" disabled={!key.trusted} busy={busy} onClick={() => void repair(probe.fingerprint)}>
              Trust and repair
            </Button>
          </>
        ) : (
          <>
            <Button onClick={onClose}>Cancel</Button>
            <Button variant="primary" type="submit" form="repair" busy={busy}>
              Log in and repair
            </Button>
          </>
        )
      }
    >
      {probe ? (
        <div className="flex flex-col gap-3">
          <HostKeyCheck host={server.host} probe={probe} acked={key.acked} onAck={key.setAcked} />
          {error && <ErrorBox>{error}</ErrorBox>}
        </div>
      ) : (
        <form
          id="repair"
          className="grid grid-cols-6 gap-3"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault()
            void repair()
          }}
        >
          <LoginFields form={login} onChange={setLogin} />
          <label className="col-span-6 flex items-start gap-2 text-[13px]">
            <input type="checkbox" className="mt-1 h-4 w-4" checked={reset} onChange={(e) => setReset(e.target.checked)} />
            <span>
              Start with new link keys
              <span className="block text-xs text-faint">Only if the agent's own state was lost.</span>
            </span>
          </label>
          {error && (
            <div className="col-span-6">
              <ErrorBox>{error}</ErrorBox>
            </div>
          )}
        </form>
      )}
    </Dialog>
  )
}

function RemoveDialog({ server, onClose, onStarted }: { server: Server; onClose: () => void; onStarted: (jobId: string) => void }) {
  const [stage, setStage] = useState<'confirm' | 'login'>('confirm')
  const [login, setLogin] = useState<LoginForm>(() => emptyLogin(server.sshPort, server.sshUser))
  const [busy, setBusy] = useState<'remove' | 'force' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const key = useHostKey(server, login)
  const probe = stage === 'login' ? key.probe : null

  const start = async (how: 'remove' | 'force', hostKey?: string) => {
    const withLogin = stage === 'login' && how === 'remove'
    if (withLogin) {
      const problem = loginError(login)
      if (problem) return setError(problem)
    }
    setBusy(how)
    setError(null)
    try {
      let req = {}
      if (how === 'force') req = { force: true }
      else if (withLogin) {
        const fingerprint = hostKey ?? (await key.check())
        if (fingerprint === null) return
        req = { login: toLogin(login, fingerprint) }
      }
      const res = await api.remove(server.id, req)
      setLogin(withoutSecrets)
      onStarted(res.jobId)
    } catch (e) {
      if (e instanceof ApiError && e.needLogin) setStage('login')
      else setError(message(e))
    } finally {
      setBusy(null)
    }
  }

  const footer = probe ? (
    <>
      <Button onClick={key.back}>Back</Button>
      <Button variant="danger" disabled={!key.trusted || !!busy} busy={busy === 'remove'} onClick={() => void start('remove', probe.fingerprint)}>
        Trust and remove
      </Button>
    </>
  ) : stage === 'confirm' ? (
    <>
      <Button onClick={onClose}>Cancel</Button>
      <Button variant="danger" busy={busy === 'remove'} onClick={() => start('remove')}>
        Remove server
      </Button>
    </>
  ) : (
    <>
      <Button onClick={onClose}>Cancel</Button>
      <Button variant="danger" type="submit" form="remove-login" busy={busy === 'remove'} disabled={!!busy}>
        Log in and remove
      </Button>
    </>
  )

  return (
    <Dialog onClose={onClose} title={`Remove ${server.name}?`} footer={footer}>
      {stage === 'confirm' && (
        <p className="text-[13px] text-muted">The agent and the proxy are removed from the server, and every user's link stops working.</p>
      )}
      {probe && <HostKeyCheck host={server.host} probe={probe} acked={key.acked} onAck={key.setAcked} />}
      {stage === 'login' && !probe && (
        <form
          id="remove-login"
          className="grid grid-cols-6 gap-3"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault()
            void start('remove')
          }}
        >
          <div className="col-span-6">
            <Notice tone="warn">The router can't reach this server's agent, so log in over SSH to remove it.</Notice>
          </div>
          <LoginFields form={login} onChange={setLogin} />
          <div className="col-span-6 flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3 text-[13px] text-muted">
            <span>No login? Remove the agent by hand later.</span>
            <Button size="sm" busy={busy === 'force'} disabled={!!busy} onClick={() => start('force')}>
              Forget without removing
            </Button>
          </div>
        </form>
      )}
      {error && (
        <div className="mt-3">
          <ErrorBox>{error}</ErrorBox>
        </div>
      )}
    </Dialog>
  )
}

function RenameDialog({ server, onClose, onRenamed }: { server: Server; onClose: () => void; onRenamed: () => void }) {
  const toast = useToast()
  const [name, setName] = useState(server.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const save = async () => {
    setBusy(true)
    try {
      const saved = await api.rename(server.id, name.trim())
      toast(`Renamed to ${saved.name}`)
      onRenamed()
      onClose()
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }
  return (
    <Dialog
      onClose={onClose}
      title="Rename server"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={busy} disabled={!name.trim()} onClick={save}>
            Save
          </Button>
        </>
      }
    >
      <Field label="Name" error={error}>
        {(p) => (
          <Input
            {...p}
            value={name}
            maxLength={100}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && name.trim() && void save()}
          />
        )}
      </Field>
    </Dialog>
  )
}

type Modal = { kind: 'rename' | 'remove' | 'repair' | 'rotate' | 'restart' | 'stop' | 'cover' | 'port' } | { kind: 'update'; what: 'agent' | 'telemt' }

const TABS: [Tab, string][] = [
  ['users', 'Users'],
  ['status', 'Status'],
  ['settings', 'Settings'],
  ['logs', 'Logs'],
]

/** "Update to 0.5.0", or "Update available" when a dev build has the same number. */
const updateText = (next: string, now: string) => (next && next !== now ? `Update to ${next}` : 'Update available')

export function ServerScreen({
  id,
  tab,
  config,
  onConfigChanged,
}: {
  id: string
  tab: Tab
  config: AppConfig | null
  onConfigChanged: () => void
}) {
  const toast = useToast()
  const jobs = useJobs()
  const sid = Number(id)
  const [restart, setRestart] = useState<{ at: number; before?: string } | null>(null)
  // a 404 is an answer here, not an error
  const detail = usePoll(
    () =>
      Number.isInteger(sid)
        ? api.server(sid).catch((e) => {
            if (e instanceof ApiError && e.status === 404) return 'gone' as const
            throw e
          })
        : Promise.resolve('gone' as const),
    restart ? 2000 : 5000,
    [sid],
  )
  const [modal, setModal] = useState<Modal | null>(null)
  const [starting, setStarting] = useState(false)
  // a removal's last event can land after the server is gone
  const last = useRef<Server | null>(null)
  if (detail.data && detail.data !== 'gone') last.current = detail.data
  const server = detail.data === 'gone' ? last.current : detail.data
  const crash = !!server?.agent?.telemt.crashLoop && server.link.state !== 'down'
  const crashLogs = usePoll(() => (crash ? api.logs(sid, 100) : Promise.resolve(null)), 30000, [sid, crash])
  const cause = crashLogs.data ? crashCause(crashLogs.data.lines) : null
  const [removing, setRemoving] = useState(false)

  // "Restarting…" lasts until a fresh report shows the proxy back, or a minute
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
        run: () => navigate({ page: 'server', id: sid, tab: 'logs' }),
      })
    }, 60000)
    return () => clearTimeout(t)
  }, [restart, toast, sid])

  // may run after this page is left, so it leans only on `last`
  const jobDone = (j: Job) => {
    const name = last.current?.name ?? 'The server'
    if (j.kind === 'uninstall' && j.state === 'ok') {
      toast(`${name} removed`)
      onConfigChanged()
      const here = parseRoute(window.location.hash)
      if (here.page === 'server' && here.id === id) navigate({ page: 'servers' })
      return
    }
    if (j.kind === 'uninstall') setRemoving(false)
    if (j.state === 'ok') toast(j.kind === 'repair' ? `${name} repaired` : jobTitle(j))
    else toast(jobTitle(j), 'error')
    void detail.reload()
  }

  const startJob = async (fn: () => Promise<{ jobId: string }>) => {
    const { jobId } = await fn()
    jobs.watch(jobId, jobDone)
    jobs.open(jobId)
    setModal(null)
  }

  if (detail.data === 'gone' && !removing) {
    return (
      <div className="flex flex-col items-start gap-3">
        <ErrorBox>That server is not in the list any more.</ErrorBox>
        <Button onClick={() => navigate({ page: 'servers' })}>
          <ChevronLeft className="h-4 w-4" /> All servers
        </Button>
      </div>
    )
  }
  if (!server) return detail.error ? <ErrorBox>{detail.error}</ErrorBox> : <Spinner />

  const reload = () => void detail.reload()
  const down = server.link.state === 'down'
  const agent = server.agent
  const close = () => setModal(null)
  const open = (t: Tab) => navigate({ page: 'server', id: server.id, tab: t })

  const startProxy = async () => {
    setStarting(true)
    try {
      await api.stopProxy(server.id, false)
      toast('Proxy started')
      await detail.reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setStarting(false)
    }
  }

  const { updates } = server
  const menu: MenuItem[] = [
    ...(updates.agent
      ? [
          {
            label: `Update agent${updates.agentVersion && updates.agentVersion !== server.agentVersion ? ` to ${updates.agentVersion}` : ''}`,
            icon: <Download />,
            disabled: down,
            hint: down ? NEEDS_LINK : undefined,
            onSelect: () => setModal({ kind: 'update', what: 'agent' }),
          },
        ]
      : []),
    ...(updates.telemt
      ? [
          {
            label: `Update proxy${updates.telemtVersion && updates.telemtVersion !== server.telemtVersion ? ` to ${updates.telemtVersion}` : ''}`,
            icon: <Download />,
            disabled: down,
            hint: down ? NEEDS_LINK : undefined,
            onSelect: () => setModal({ kind: 'update', what: 'telemt' }),
          },
        ]
      : []),
    ...(server.registered
      ? [
          {
            label: 'Renew link keys',
            icon: <KeyRound />,
            disabled: down,
            hint: down ? `Users keep their links. ${NEEDS_LINK}` : 'Users keep their links',
            onSelect: () => setModal({ kind: 'rotate' }),
          },
        ]
      : []),
    { label: 'Rename', icon: <Pencil />, onSelect: () => setModal({ kind: 'rename' }) },
    { label: 'Remove server…', icon: <Trash2 />, danger: true, divide: true, onSelect: () => setModal({ kind: 'remove' }) },
  ]

  const chip = (what: 'agent' | 'telemt') => {
    const text = updateText(what === 'agent' ? updates.agentVersion : updates.telemtVersion, what === 'agent' ? server.agentVersion : server.telemtVersion)
    return (
      <button
        type="button"
        disabled={down}
        title={down ? NEEDS_LINK : undefined}
        onClick={() => setModal({ kind: 'update', what })}
        // small to look at, but a 44 px target for a thumb
        className="relative inline-flex h-6 items-center gap-1 whitespace-nowrap rounded-full border border-border-strong px-2 text-[11.5px] font-medium text-foreground before:absolute before:-inset-x-1 before:-inset-y-2.5 before:content-[''] hover:bg-card2 disabled:cursor-not-allowed disabled:opacity-60 sm:h-5 sm:before:hidden"
      >
        <Download className="h-3 w-3" />
        {text}
      </button>
    )
  }

  const n = openConnections(server)
  const updateWhat = modal?.kind === 'update' ? modal.what : null

  return (
    <div className="flex flex-col gap-5">
      <button
        type="button"
        onClick={() => navigate({ page: 'servers' })}
        className="-my-2 inline-flex h-11 w-fit items-center gap-1.5 text-[13px] text-muted hover:text-foreground sm:my-0 sm:h-auto"
      >
        <ChevronLeft className="h-4 w-4" /> All servers
      </button>

      <Card className="flex flex-col gap-4 p-4 sm:flex-row sm:items-center">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-lg font-semibold">{server.name}</h1>
            <Badge tone={linkTone(server.link.state)}>link {server.link.state}</Badge>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
            <span className="font-mono">{server.host}</span>
            {server.coverDomain && <span>cover {server.coverDomain}</span>}
            {server.agentVersion && (
              <span className="inline-flex items-center gap-1.5">
                agent {server.agentVersion}
                {updates.agent && chip('agent')}
              </span>
            )}
            {server.telemtVersion && (
              <span className="inline-flex items-center gap-1.5">
                proxy {server.telemtVersion}
                {updates.telemt && chip('telemt')}
              </span>
            )}
            {agent && !down && <span>up {formatDuration(agent.uptimeSec)}</span>}
          </div>
        </div>
        <div role="group" aria-label="Server actions" className="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap">
          {server.registered && (
            <>
              <Button
                size="sm"
                busy={!!restart}
                disabled={down || server.proxyStopped}
                title={down ? NEEDS_LINK : server.proxyStopped ? 'Start the proxy first' : undefined}
                onClick={() => setModal({ kind: 'restart' })}
              >
                {!restart && <RefreshCw className="h-3.5 w-3.5" />} {restart ? 'Restarting…' : 'Restart proxy'}
              </Button>
              {server.proxyStopped ? (
                <Button size="sm" variant="primary" busy={starting} onClick={startProxy}>
                  <Play className="h-3.5 w-3.5" /> Start proxy
                </Button>
              ) : (
                <Button size="sm" disabled={down} title={down ? NEEDS_LINK : undefined} onClick={() => setModal({ kind: 'stop' })}>
                  <Square className="h-3.5 w-3.5" /> Stop proxy
                </Button>
              )}
              <Button size="sm" onClick={() => setModal({ kind: 'repair' })}>
                <Wrench className="h-3.5 w-3.5" /> Repair
              </Button>
            </>
          )}
          <Menu label="More actions" size="sm" className={cx('sm:w-8 sm:px-0', !server.registered && 'col-span-2')} items={menu}>
            <Ellipsis className="h-4 w-4" />
            <span className="sm:hidden">More</span>
          </Menu>
        </div>
      </Card>

      {detail.error && <ErrorBox>{detail.error}</ErrorBox>}
      <Attention
        server={server}
        tab={tab}
        cause={cause}
        actions={{
          repair: () => setModal({ kind: 'repair' }),
          restart: () => setModal({ kind: 'restart' }),
          start: () => void startProxy(),
          remove: () => setModal({ kind: 'remove' }),
          changeCover: () => setModal({ kind: 'cover' }),
          open,
        }}
      />

      <Tabs label="Server sections" idBase="srv" tabs={TABS} value={tab} onChange={open} />

      <div role="tabpanel" id="srv-panel" aria-labelledby={`srv-${tab}`}>
        {tab === 'users' && <UsersTab server={server} />}
        {tab === 'status' && <StatusTab server={server} />}
        {tab === 'settings' && (
          <SettingsTab
            server={server}
            config={config}
            onChangeCover={() => setModal({ kind: 'cover' })}
            onChangePort={() => setModal({ kind: 'port' })}
            onChanged={() => {
              reload()
              onConfigChanged()
            }}
          />
        )}
        {tab === 'logs' && <LogsTab server={server} />}
      </div>

      {modal?.kind === 'rename' && <RenameDialog server={server} onClose={close} onRenamed={reload} />}
      {modal?.kind === 'remove' && (
        <RemoveDialog
          server={server}
          onClose={close}
          onStarted={(jobId) => {
            setRemoving(true)
            void startJob(async () => ({ jobId }))
          }}
        />
      )}
      {modal?.kind === 'repair' && <RepairDialog server={server} onClose={close} onStarted={(jobId) => void startJob(async () => ({ jobId }))} />}
      {modal?.kind === 'rotate' && (
        <Confirm title="Renew the link keys?" action="Renew link keys" onClose={close} onConfirm={() => startJob(() => api.rotate(server.id))}>
          <p>The agent and the router switch to new link keys. Users keep their links.</p>
        </Confirm>
      )}
      {modal?.kind === 'restart' && (
        <Confirm
          title={`Restart the proxy on ${server.name}?`}
          action="Restart proxy"
          onClose={close}
          onConfirm={async () => {
            await api.restart(server.id)
            setRestart({ at: Date.now(), before: server.agent?.reportedAt })
          }}
        >
          <Impact n={n}>{dropLine(server)}</Impact>
          <p>The button shows "Restarting…" until the proxy reports back. If it isn't back within a minute, you'll see why.</p>
        </Confirm>
      )}
      {modal?.kind === 'stop' && (
        <Confirm
          danger
          title={`Stop the proxy on ${server.name}?`}
          action="Stop proxy"
          onClose={close}
          onConfirm={async () => {
            await api.stopProxy(server.id, true)
            toast('Proxy stopped')
            reload()
          }}
        >
          <Impact n={n}>{dropLine(server).replace(/ Telegram reconnects.*$/, '')}</Impact>
          <p>It stays off, across restarts and repairs, until you press Start proxy.</p>
        </Confirm>
      )}
      {updateWhat && (
        <Confirm
          title={`Update the ${updateWhat === 'agent' ? 'agent' : 'proxy'} on ${server.name}${
            (updateWhat === 'agent' ? updates.agentVersion : updates.telemtVersion) ? ` to ${updateWhat === 'agent' ? updates.agentVersion : updates.telemtVersion}` : ''
          }?`}
          action={updateWhat === 'agent' ? 'Update agent' : 'Update proxy'}
          onClose={close}
          onConfirm={() => startJob(() => api.update(server.id, updateWhat))}
        >
          <Impact n={n}>{dropLine(server)}</Impact>
          <p>It takes a minute or two. The job tray at the top shows how it goes.</p>
        </Confirm>
      )}
      {modal?.kind === 'cover' && <CoverDialog server={server} onClose={close} onChanged={reload} />}
      {modal?.kind === 'port' && (
        <LinkPortDialog server={server} onClose={close} onStart={(port) => startJob(() => api.linkPort(server.id, port))} />
      )}
    </div>
  )
}
