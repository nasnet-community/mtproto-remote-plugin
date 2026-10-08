import { useRef, useState } from 'react'
import {
  ChevronLeft,
  Download,
  Ellipsis,
  KeyRound,
  Pencil,
  RefreshCw,
  Trash2,
  Wrench,
} from '../components/icons'
import { JobSteps } from '../components/job'
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
import { formatDuration, heldReason, linkTone, relativeTime } from '../lib/format'
import { useBackgroundJob } from '../hooks/useJob'
import { usePoll } from '../hooks/usePoll'
import { navigate, parseRoute, type Tab } from '../hooks/useRoute'
import {
  Badge,
  Button,
  Card,
  Dialog,
  ErrorBox,
  Field,
  Input,
  Menu,
  Notice,
  Spinner,
  cx,
  useToast,
} from '../components/ui'
import { proxyProblem } from './ServersScreen'
import { UsersTab } from './UsersTab'
import { SettingsTab } from './SettingsTab'
import { StatusTab } from './StatusTab'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

type JobKind = Exclude<Job['kind'], 'install'>

const JOB_COPY: Record<JobKind, { running: string; ok: string; failed: string }> = {
  'update-agent': { running: 'Updating the agent', ok: 'Agent updated', failed: 'Update failed' },
  'update-telemt': { running: 'Updating the proxy', ok: 'Proxy updated', failed: 'Update failed' },
  rotate: { running: 'Rotating keys', ok: 'Keys rotated', failed: 'Rotation failed' },
  repair: { running: 'Repairing', ok: 'Server repaired', failed: 'Repair failed' },
  uninstall: { running: 'Removing', ok: 'Server removed', failed: 'Removal failed' },
}

function JobDialog({ job, kind, onClose }: { job: Job | null; kind: JobKind; onClose: () => void }) {
  const copy = JOB_COPY[kind]
  const title = job?.state === 'ok' ? copy.ok : job?.state === 'failed' ? copy.failed : copy.running
  return (
    <Dialog
      open
      onClose={onClose}
      title={title}
      footer={
        job && job.state !== 'running' ? (
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        ) : undefined
      }
    >
      <JobSteps job={job} />
    </Dialog>
  )
}

/**
 * Looks at the server's SSH host key before a login goes out: a known key goes
 * straight on, a new or changed one is shown first (a rebuilt VPS has a new one).
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

function RepairDialog({
  server,
  onClose,
  onStarted,
}: {
  server: Server
  onClose: () => void
  onStarted: (jobId: string) => void
}) {
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
      open
      onClose={onClose}
      title={`Repair ${server.name}`}
      description={probe ? undefined : 'Reinstalls the agent over SSH; settings and users stay.'}
      footer={
        probe ? (
          <>
            <Button onClick={key.back}>Back</Button>
            <Button
              variant={probe.status === 'changed' ? 'danger' : 'primary'}
              disabled={!key.trusted}
              busy={busy}
              onClick={() => void repair(probe.fingerprint)}
            >
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
            <input type="checkbox" className="mt-1" checked={reset} onChange={(e) => setReset(e.target.checked)} />
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

function RemoveDialog({
  server,
  onClose,
  onStarted,
}: {
  server: Server
  onClose: () => void
  onStarted: (jobId: string) => void
}) {
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
      <Button
        variant="danger"
        disabled={!key.trusted || !!busy}
        busy={busy === 'remove'}
        onClick={() => void start('remove', probe.fingerprint)}
      >
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
    <Dialog open onClose={onClose} title={`Remove ${server.name}?`} footer={footer}>
      {stage === 'confirm' && (
        <p className="text-[13px] text-muted">
          The agent and the proxy are removed from the server, and every user's link stops
          working.
        </p>
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
            <Notice tone="warn">
              The router cannot reach this server's agent, so log in over SSH to remove it.
            </Notice>
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

function RenameDialog({
  server,
  onClose,
  onRenamed,
}: {
  server: Server
  onClose: () => void
  onRenamed: () => void
}) {
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
      open
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
        {(id) => (
          <Input
            id={id}
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

function RotateDialog({ onClose, onConfirm }: { onClose: () => void; onConfirm: () => Promise<void> }) {
  const [busy, setBusy] = useState(false)
  return (
    <Dialog
      open
      onClose={onClose}
      title="Rotate the link keys?"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            busy={busy}
            onClick={async () => {
              setBusy(true)
              await onConfirm()
              setBusy(false)
            }}
          >
            Rotate keys
          </Button>
        </>
      }
    >
      <p className="text-[13px] text-muted">
        The agent and the router switch to new link keys. Users and their links stay the same.
      </p>
    </Dialog>
  )
}

function LogsTab({ server }: { server: Server }) {
  const { data, error, loading, reload } = usePoll(() => api.logs(server.id, 300), 10000, [server.id])
  return (
    <Card className="flex flex-col gap-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-[13px] text-muted">The agent's last 300 lines, kept in its memory.</p>
        <Button size="sm" onClick={() => void reload()} busy={loading}>
          <RefreshCw className="h-3.5 w-3.5" /> Refresh
        </Button>
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
      {!data && !error && <Spinner />}
      {data && (
        <pre className="max-h-[65vh] overflow-auto whitespace-pre-wrap break-words rounded-md bg-card2 p-3 font-mono text-[11.5px] leading-relaxed">
          {data.lines.join('\n').trim() || 'No log lines yet.'}
        </pre>
      )}
    </Card>
  )
}

type Modal = { kind: 'rename' | 'remove' | 'repair' | 'rotate' | 'job' }

const TABS: Tab[] = ['users', 'status', 'settings', 'logs']

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
  const sid = Number(id)
  // a 404 is an answer here, not an error
  const detail = usePoll(
    () =>
      Number.isInteger(sid)
        ? api.server(sid).catch((e) => {
            if (e instanceof ApiError && e.status === 404) return 'gone' as const
            throw e
          })
        : Promise.resolve('gone' as const),
    5000,
    [sid],
  )
  const [modal, setModal] = useState<Modal | null>(null)
  const [acting, setActing] = useState<string | null>(null)
  // a removal's last event can land after the server is gone
  const last = useRef<Server | null>(null)
  if (detail.data && detail.data !== 'gone') last.current = detail.data

  // may run after this page is left, so it leans only on `last`
  const jobDone = (j: Job) => {
    const name = last.current?.name ?? 'The server'
    const copy = JOB_COPY[j.kind as JobKind]
    if (j.kind === 'uninstall' && j.state === 'ok') {
      toast(`${name} removed`)
      onConfigChanged()
      const here = parseRoute(window.location.hash)
      if (here.page === 'server' && here.id === id) navigate({ page: 'servers' })
      return
    }
    if (j.state === 'ok') toast(j.kind === 'repair' ? `${name} repaired` : copy.ok)
    else toast(copy.failed, 'error')
    void detail.reload()
  }
  const bg = useBackgroundJob<JobKind>(jobDone)

  if (detail.data === 'gone' && !bg.running) {
    return (
      <div className="flex flex-col items-start gap-3">
        <ErrorBox>That server is not in the list any more.</ErrorBox>
        <Button onClick={() => navigate({ page: 'servers' })}>
          <ChevronLeft className="h-4 w-4" /> All servers
        </Button>
      </div>
    )
  }
  const server = detail.data === 'gone' ? last.current : detail.data
  if (!server) return detail.error ? <ErrorBox>{detail.error}</ErrorBox> : <Spinner />

  const reload = () => void detail.reload()
  const down = server.link.state === 'down'
  const agent = server.agent
  const problem = proxyProblem(server)

  const restart = async () => {
    setActing('restart')
    try {
      await api.restart(server.id)
      toast('Proxy restarted')
      await detail.reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setActing(null)
    }
  }

  const watch = (jobId: string, kind: JobKind) => {
    bg.follow(jobId, kind)
    setModal({ kind: 'job' })
  }

  const startJob = async (kind: JobKind, fn: () => Promise<{ jobId: string }>) => {
    try {
      watch((await fn()).jobId, kind)
    } catch (e) {
      toast(message(e), 'error')
      setModal(null)
    }
  }

  const closeJob = () => {
    if (bg.running && bg.followed) toast(`${JOB_COPY[bg.followed.kind].running} in the background`)
    setModal(null)
  }

  return (
    <div className="flex flex-col gap-5">
      <button
        type="button"
        onClick={() => navigate({ page: 'servers' })}
        className="inline-flex w-fit items-center gap-1.5 text-[13px] text-muted hover:text-foreground"
      >
        <ChevronLeft className="h-4 w-4" /> All servers
      </button>

      <Card className="flex flex-col gap-4 p-4 sm:flex-row sm:items-center">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-lg font-semibold">{server.name}</h1>
            <Badge tone={linkTone(server.link.state)}>link {server.link.state}</Badge>
            {problem && <Badge tone="danger">{problem}</Badge>}
          </div>
          <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
            <span className="font-mono">{server.host}</span>
            <span>cover {server.coverDomain}</span>
            {server.agentVersion && <span>agent {server.agentVersion}</span>}
            {server.telemtVersion && <span>proxy {server.telemtVersion}</span>}
            {agent && !down && <span>up {formatDuration(agent.uptimeSec)}</span>}
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          {bg.running && bg.followed && modal?.kind !== 'job' && (
            <Button size="sm" onClick={() => setModal({ kind: 'job' })}>
              <Spinner className="h-3.5 w-3.5" /> {JOB_COPY[bg.followed.kind].running}
            </Button>
          )}
          <Button size="sm" busy={acting === 'restart'} disabled={down} onClick={restart}>
            <RefreshCw className="h-3.5 w-3.5" /> Restart proxy
          </Button>
          {server.registered && (
            <Button size="sm" onClick={() => setModal({ kind: 'repair' })}>
              <Wrench className="h-3.5 w-3.5" /> Repair
            </Button>
          )}
          <Menu
            trigger={
              <Button size="icon" aria-label="More actions">
                <Ellipsis className="h-4 w-4" />
              </Button>
            }
            items={[
              {
                label: 'Update agent',
                icon: <Download />,
                onSelect: () => void startJob('update-agent', () => api.update(server.id, 'agent')),
              },
              {
                label: 'Update proxy',
                icon: <Download />,
                onSelect: () => void startJob('update-telemt', () => api.update(server.id, 'telemt')),
              },
              { label: 'Rotate keys', icon: <KeyRound />, onSelect: () => setModal({ kind: 'rotate' }) },
              { label: 'Rename', icon: <Pencil />, onSelect: () => setModal({ kind: 'rename' }) },
              {
                label: 'Remove server',
                icon: <Trash2 />,
                danger: true,
                onSelect: () => setModal({ kind: 'remove' }),
              },
            ]}
          />
        </div>
      </Card>

      {detail.error && <ErrorBox>{detail.error}</ErrorBox>}
      {!server.registered ? (
        <Notice tone="danger">
          This server's install never finished, so it can't be repaired. Remove it, then add it again.
        </Notice>
      ) : down ? (
        <Notice tone="danger">
          The router cannot reach this server's agent
          {server.lastSeenAt ? ` (last seen ${relativeTime(server.lastSeenAt)})` : ''}. Check that
          the server is up and its address is in the domestic address list, or run a repair.
        </Notice>
      ) : server.link.state === 'degraded' ? (
        <Notice tone="warn">
          {server.link.up} of {server.link.total} link connections are up.
        </Notice>
      ) : null}
      {agent?.telemt.crashLoop ? (
        <Notice tone="danger">
          The proxy keeps crashing{agent.telemt.lastExit ? ` (${agent.telemt.lastExit})` : ''}.
          Check the logs, then restart it.
        </Notice>
      ) : agent && !agent.telemt.running ? (
        <Notice tone="warn">The proxy is not running{heldReason(agent.telemt) ? `: ${heldReason(agent.telemt)}` : ''}.</Notice>
      ) : null}
      {!agent && !down && <Notice>Waiting for the agent's first status report.</Notice>}
      {!down && !server.sync.healthy && server.sync.lastPushError && (
        <Notice tone="warn">Settings are not applied yet: {server.sync.lastPushError}</Notice>
      )}

      <div className="flex gap-1 overflow-x-auto border-b border-border">
        {TABS.map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => navigate({ page: 'server', id: server.id, tab: t })}
            className={cx(
              '-mb-px border-b-2 px-3 py-2 text-[13px] font-medium',
              tab === t
                ? 'border-accent text-foreground'
                : 'border-transparent text-muted hover:text-foreground',
            )}
          >
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>

      {tab === 'users' && <UsersTab server={server} />}
      {tab === 'status' && <StatusTab server={server} />}
      {tab === 'settings' && (
        <SettingsTab
          server={server}
          config={config}
          onChanged={() => {
            reload()
            onConfigChanged()
          }}
        />
      )}
      {tab === 'logs' && <LogsTab server={server} />}

      {modal?.kind === 'rename' && (
        <RenameDialog server={server} onClose={() => setModal(null)} onRenamed={reload} />
      )}
      {modal?.kind === 'remove' && (
        <RemoveDialog server={server} onClose={() => setModal(null)} onStarted={(jobId) => watch(jobId, 'uninstall')} />
      )}
      {modal?.kind === 'repair' && (
        <RepairDialog server={server} onClose={() => setModal(null)} onStarted={(jobId) => watch(jobId, 'repair')} />
      )}
      {modal?.kind === 'rotate' && (
        <RotateDialog
          onClose={() => setModal(null)}
          onConfirm={() => startJob('rotate', () => api.rotate(server.id))}
        />
      )}
      {modal?.kind === 'job' && bg.followed && (
        <JobDialog key={bg.followed.jobId} job={bg.followed.job} kind={bg.followed.kind} onClose={closeJob} />
      )}
    </div>
  )
}
