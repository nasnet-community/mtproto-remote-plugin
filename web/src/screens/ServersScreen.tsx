import { useState } from 'react'
import { ChevronRight, Plus, TelegramMtprotoLogo, Users } from '../components/icons'
import { api, type AppConfig, type Server } from '../lib/api'
import { linkTone, plural, relativeTime } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate } from '../hooks/useRoute'
import { Badge, Button, Card, ErrorBox, Notice, Spinner } from '../components/ui'
import { AddServerDialog } from './AddServerDialog'

/** How the proxy on a server is doing, when it needs attention. */
export function proxyProblem(server: Server): string | null {
  const t = server.agent?.telemt
  if (!t) return null
  if (t.crashLoop) return 'proxy keeps crashing'
  if (!t.running) return 'proxy stopped'
  return null
}

function ServerCard({ server }: { server: Server }) {
  const problem = proxyProblem(server)
  const down = server.link.state === 'down'
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
            <div className="truncate font-mono text-xs text-muted">{server.host}</div>
          </div>
          <Badge tone={linkTone(server.link.state)}>link {server.link.state}</Badge>
        </div>
        <div className="mt-auto flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
          <span className="inline-flex items-center gap-1.5">
            <Users className="h-3.5 w-3.5" />
            {plural(server.userCount, 'user')}
          </span>
          {server.agentVersion && <span>agent {server.agentVersion}</span>}
          {problem && <span className="text-status-danger">{problem}</span>}
          {down && server.lastSeenAt && <span>last seen {relativeTime(server.lastSeenAt)}</span>}
          <ChevronRight className="ml-auto h-4 w-4 text-faint transition group-hover:text-foreground" />
        </div>
      </Card>
    </button>
  )
}

export function ServersScreen({
  config,
  onChanged,
}: {
  config: AppConfig | null
  onChanged: () => void
}) {
  const { data: servers, error, loading, reload } = usePoll(api.servers, 10000)
  const [adding, setAdding] = useState(false)

  const max = config?.maxServers ?? 0
  const count = servers?.length ?? config?.serverCount ?? 0
  const full = max > 0 && count >= max

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Servers</h1>
          <p className="text-[13px] text-muted">
            Each server's proxy reaches Telegram through this router.
          </p>
        </div>
        <div className="flex items-center gap-3">
          {max > 0 && (
            <span className="text-xs text-faint">
              {count} of {plural(max, 'server')}
            </span>
          )}
          <Button variant="primary" disabled={full} onClick={() => setAdding(true)}>
            <Plus className="h-4 w-4" /> Add server
          </Button>
        </div>
      </div>

      {full && (
        <Notice>
          <b>This router runs at most {plural(max, 'server')}.</b>{' '}
          {max === 1
            ? 'Small routers run one server, to keep memory free.'
            : "Each server's link uses router memory."}{' '}
          Remove one to add another.
        </Notice>
      )}
      {error && <ErrorBox>{error}</ErrorBox>}
      {loading && !servers && <Spinner />}

      {servers && servers.length === 0 && (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <TelegramMtprotoLogo size={48} />
          <h2 className="text-base font-semibold">No servers yet</h2>
          <p className="max-w-md text-[13px] text-muted">
            Add a Linux server over SSH. The login is used once, never stored.
          </p>
          <Button variant="primary" disabled={full} onClick={() => setAdding(true)}>
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
        config={config}
        onClose={() => setAdding(false)}
        onInstalled={() => {
          void reload()
          onChanged()
        }}
        onOpenServer={(id) => {
          setAdding(false)
          navigate({ page: 'server', id, tab: 'users' })
        }}
      />
    </div>
  )
}
