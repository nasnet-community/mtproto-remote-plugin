import { useState } from 'react'
import {
  ChevronLeft,
  Download,
  Ellipsis,
  Pencil,
  Power,
  PowerOff,
  RefreshCw,
  ScrollText,
  Trash2,
} from '../components/icons'
import { api, type AppConfig, type Server } from '../lib/api'
import { formatDuration } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate } from '../hooks/useRoute'
import {
  Badge,
  Button,
  Card,
  Dialog,
  ErrorBox,
  Field,
  Input,
  Menu,
  Spinner,
  cx,
  useToast,
} from '../components/ui'
import { serviceTone } from './ServersScreen'
import { UsersTab } from './UsersTab'
import { SettingsTab } from './SettingsTab'

function LogsDialog({ server, onClose }: { server: Server; onClose: () => void }) {
  const { data, error, loading, reload } = usePoll(() => api.logs(server.id, 300), 10000, [server.id])
  return (
    <Dialog
      open
      wide
      onClose={onClose}
      title={`Logs — ${server.name}`}
      description="The last 300 lines from the journal; refreshes every 10 seconds."
      footer={
        <Button onClick={() => void reload()} busy={loading}>
          <RefreshCw className="h-4 w-4" /> Refresh
        </Button>
      }
    >
      {error && <ErrorBox>{error}</ErrorBox>}
      {!data && !error && <Spinner />}
      {data && (
        <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-md bg-card2 p-3 font-mono text-[11.5px] leading-relaxed">
          {data.logs.trim() || 'No log lines yet.'}
        </pre>
      )}
    </Dialog>
  )
}

function RemoveDialog({ server, onClose }: { server: Server; onClose: () => void }) {
  const toast = useToast()
  const [uninstall, setUninstall] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const remove = async () => {
    setBusy(true)
    setError(null)
    try {
      const res = await api.remove(server.id, uninstall)
      toast(res.warning ?? `${server.name} removed`, res.warning ? 'error' : 'ok')
      navigate({ page: 'servers' })
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
    }
  }
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Remove ${server.name}?`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" busy={busy} onClick={remove}>
            {uninstall ? 'Uninstall and remove' : 'Remove from list'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-[13px]">
        <label className="flex gap-3 rounded-md border border-border p-3">
          <input type="radio" checked={uninstall} onChange={() => setUninstall(true)} />
          <span>
            <b>Uninstall the proxy</b>
            <span className="block text-muted">
              Stops the proxy and deletes it, its config and every user from the server.
              All links stop working.
            </span>
          </span>
        </label>
        <label className="flex gap-3 rounded-md border border-border p-3">
          <input type="radio" checked={!uninstall} onChange={() => setUninstall(false)} />
          <span>
            <b>Only remove from this list</b>
            <span className="block text-muted">
              The proxy keeps running with its users. Add the server again later to manage it.
            </span>
          </span>
        </label>
        <p className="text-muted">Either way, the router's key is removed from the server.</p>
        {error && <ErrorBox>{error}</ErrorBox>}
      </div>
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
  const [name, setName] = useState(server.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const save = async () => {
    setBusy(true)
    try {
      await api.rename(server.id, name.trim())
      onRenamed()
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
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
            maxLength={64}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && name.trim() && void save()}
          />
        )}
      </Field>
    </Dialog>
  )
}

export function ServerScreen({
  id,
  tab,
  config,
}: {
  id: string
  tab: 'users' | 'settings'
  config: AppConfig | null
}) {
  const toast = useToast()
  const servers = usePoll(api.servers, 60000)
  const status = usePoll(() => api.status(id), 10000, [id])
  const [dialog, setDialog] = useState<'logs' | 'remove' | 'rename' | null>(null)
  const [acting, setActing] = useState<string | null>(null)

  const server = servers.data?.find((s) => s.id === id)
  if (servers.data && !server) {
    return (
      <div className="flex flex-col items-start gap-3">
        <ErrorBox>That server is not in the list any more.</ErrorBox>
        <Button onClick={() => navigate({ page: 'servers' })}>
          <ChevronLeft className="h-4 w-4" /> All servers
        </Button>
      </div>
    )
  }
  if (!server) return <Spinner />

  const service = status.data?.service
  const running = service === 'active'

  const act = async (label: string, fn: () => Promise<unknown>, done?: string) => {
    setActing(label)
    try {
      await fn()
      if (done) toast(done)
      await status.reload()
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e), 'error')
    } finally {
      setActing(null)
    }
  }

  const info = status.data?.info
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
            <Badge tone={status.error ? 'danger' : serviceTone(service)}>
              {status.error ? 'unreachable' : (service ?? 'checking…')}
            </Badge>
          </div>
          <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
            <span className="font-mono">
              {server.username}@{server.host}:{server.sshPort}
            </span>
            <span>proxy port {server.proxyPort}</span>
            {info?.version && <span>version {info.version}</span>}
            {info?.uptime_seconds !== undefined && (
              <span>up {formatDuration(info.uptime_seconds)}</span>
            )}
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          {running ? (
            <Button
              size="sm"
              busy={acting === 'stop'}
              disabled={!!acting}
              className="enabled:hover:!border-status-danger enabled:hover:!bg-status-danger enabled:hover:!text-white"
              onClick={() => act('stop', () => api.service(id, 'stop'), 'Proxy stopped')}
            >
              <PowerOff className="h-3.5 w-3.5" /> Stop
            </Button>
          ) : (
            <Button
              size="sm"
              busy={acting === 'start'}
              disabled={!!acting}
              onClick={() => act('start', () => api.service(id, 'start'), 'Proxy started')}
            >
              <Power className="h-3.5 w-3.5" /> Start
            </Button>
          )}
          <Button
            size="sm"
            busy={acting === 'restart'}
            disabled={!!acting}
            onClick={() => act('restart', () => api.service(id, 'restart'), 'Proxy restarted')}
          >
            <RefreshCw className="h-3.5 w-3.5" /> Restart
          </Button>
          <Button size="sm" onClick={() => setDialog('logs')}>
            <ScrollText className="h-3.5 w-3.5" /> Logs
          </Button>
          <Menu
            trigger={
              <Button size="icon" aria-label="More actions" busy={acting === 'update'}>
                <Ellipsis className="h-4 w-4" />
              </Button>
            }
            items={[
              {
                label: 'Update proxy',
                icon: <Download />,
                onSelect: () =>
                  act(
                    'update',
                    async () => {
                      const res = await api.update(id)
                      toast(`Updated to version ${res.release}`)
                    },
                  ),
              },
              { label: 'Rename', icon: <Pencil />, onSelect: () => setDialog('rename') },
              {
                label: 'Remove server',
                icon: <Trash2 />,
                danger: true,
                onSelect: () => setDialog('remove'),
              },
            ]}
          />
        </div>
      </Card>

      {status.error && (
        <ErrorBox>
          {status.error}
          {status.error.includes('router key') &&
            '\nAdd the server again to give the router a new key.'}
        </ErrorBox>
      )}
      {status.data?.apiError && running && (
        <ErrorBox>The proxy is running but did not answer: {status.data.apiError}</ErrorBox>
      )}

      <div className="flex gap-1 border-b border-border">
        {(['users', 'settings'] as const).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => navigate({ page: 'server', id, tab: t })}
            className={cx(
              '-mb-px border-b-2 px-3 py-2 text-[13px] font-medium capitalize',
              tab === t
                ? 'border-accent text-foreground'
                : 'border-transparent text-muted hover:text-foreground',
            )}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === 'users' ? (
        <UsersTab server={server} />
      ) : (
        <SettingsTab server={server} config={config} version={info?.version} />
      )}

      {dialog === 'logs' && <LogsDialog server={server} onClose={() => setDialog(null)} />}
      {dialog === 'remove' && <RemoveDialog server={server} onClose={() => setDialog(null)} />}
      {dialog === 'rename' && (
        <RenameDialog
          server={server}
          onClose={() => setDialog(null)}
          onRenamed={() => void servers.reload()}
        />
      )}
    </div>
  )
}
