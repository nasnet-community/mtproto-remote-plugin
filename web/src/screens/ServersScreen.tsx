import { useState } from 'react'
import { ChevronRight, Plus, TelegramMtprotoLogo, Users } from '../components/icons'
import { api, type Server } from '../lib/api'
import { formatDuration } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate } from '../hooks/useRoute'
import { Badge, Button, Card, ErrorBox, Spinner } from '../components/ui'
import { AddServerDialog } from './AddServerDialog'

export function serviceTone(service: string | undefined) {
  if (service === 'active') return 'online' as const
  if (service === 'activating' || service === 'reloading') return 'warn' as const
  if (!service) return 'idle' as const
  return 'danger' as const
}

function ServerCard({ server }: { server: Server }) {
  const { data: status, error } = usePoll(() => api.status(server.id), 15000, [server.id])
  const tone = error ? 'danger' : serviceTone(status?.service)
  const label = error ? 'unreachable' : (status?.service ?? 'checking…')
  return (
    <button
      type="button"
      onClick={() => navigate({ page: 'server', id: server.id, tab: 'users' })}
      className="group text-left"
    >
      <Card className="flex h-full flex-col gap-4 p-4 transition group-hover:border-border-strong">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="truncate font-semibold">{server.name}</div>
            <div className="truncate font-mono text-xs text-muted">
              {server.host}:{server.proxyPort}
            </div>
          </div>
          <Badge tone={tone}>{label}</Badge>
        </div>
        <div className="mt-auto flex items-center gap-4 text-xs text-muted">
          <span className="inline-flex items-center gap-1.5">
            <Users className="h-3.5 w-3.5" />
            {status?.summary?.configured_users ?? '–'} users
          </span>
          {status?.info?.version && <span>v{status.info.version}</span>}
          {status?.info?.uptime_seconds !== undefined && (
            <span>up {formatDuration(status.info.uptime_seconds)}</span>
          )}
          <ChevronRight className="ml-auto h-4 w-4 text-faint transition group-hover:text-foreground" />
        </div>
        {error && <p className="text-xs text-status-danger line-clamp-2">{error}</p>}
      </Card>
    </button>
  )
}

export function ServersScreen() {
  const { data: servers, error, loading, reload } = usePoll(api.servers, 30000)
  const [adding, setAdding] = useState(false)

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Servers</h1>
          <p className="text-[13px] text-muted">
            Each server runs its own proxy with its own users.
          </p>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <Plus className="h-4 w-4" /> Add server
        </Button>
      </div>

      {error && <ErrorBox>{error}</ErrorBox>}
      {loading && !servers && <Spinner />}

      {servers && servers.length === 0 && (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <TelegramMtprotoLogo size={48} />
          <h2 className="text-base font-semibold">No servers yet</h2>
          <p className="max-w-md text-[13px] text-muted">
            Add a Linux server over SSH. Your password is used once, never stored.
          </p>
          <Button variant="primary" onClick={() => setAdding(true)}>
            <Plus className="h-4 w-4" /> Add your first server
          </Button>
        </Card>
      )}

      {servers && servers.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {servers.map((s) => (
            <ServerCard key={s.id} server={s} />
          ))}
        </div>
      )}

      <AddServerDialog
        open={adding}
        onClose={() => setAdding(false)}
        onInstalled={(id) => {
          setAdding(false)
          void reload()
          navigate({ page: 'server', id, tab: 'users' })
        }}
      />
    </div>
  )
}
