import { useState } from 'react'
import { ChevronRight, Download, Plus, TelegramMtprotoLogo, TriangleAlert } from '../components/icons'
import { api, type AppConfig, type Server } from '../lib/api'
import { formatBytes, health, plural } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { navigate } from '../hooks/useRoute'
import { Badge, Button, Card, ErrorBox, Notice, Spinner, cx } from '../components/ui'
import { AddServerDialog } from './AddServerDialog'

/** A small chip that says an update is waiting. */
export function UpdateChip({ children }: { children: string }) {
  return (
    <span className="inline-flex items-center gap-1 whitespace-nowrap rounded-full border border-border-strong px-2 py-px text-[11.5px] font-medium text-foreground">
      <Download className="h-3 w-3" />
      {children}
    </span>
  )
}

function ServerCard({ server }: { server: Server }) {
  const h = health(server)
  const u = server.usage
  return (
    <button type="button" onClick={() => navigate({ page: 'server', id: server.id, tab: 'users' })} className="group text-left">
      <Card className="flex h-full flex-col gap-3 p-4 transition group-hover:border-border-strong">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="truncate font-semibold">{server.name}</div>
            <div className="truncate font-mono text-xs text-muted">{server.host}</div>
          </div>
          <Badge tone={h.tone}>{h.label}</Badge>
        </div>
        <div className="tabular flex flex-wrap items-baseline gap-x-6 gap-y-1 text-[13px] text-muted">
          {u.live ? (
            <span>
              <b className="text-lg font-semibold text-foreground">{u.usersOnline}</b> of {server.userCount} online
            </span>
          ) : (
            <span>
              <b className="text-lg font-semibold text-foreground">{server.userCount}</b> {server.userCount === 1 ? 'user' : 'users'}
            </span>
          )}
          <span>
            <b className="text-lg font-semibold text-foreground">{formatBytes(u.todayUp + u.todayDown)}</b> today
          </span>
        </div>
        {h.reason && h.tone !== 'online' && (
          <div className={cx('flex items-start gap-2 text-[13px]', h.tone === 'danger' ? 'text-bad-text' : 'text-warn-text')}>
            <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0" />
            {h.reason}
          </div>
        )}
        <div className="mt-auto flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
          {server.registered && (
            <span>
              agent {server.agentVersion || '–'} · proxy {server.telemtVersion || '–'}
            </span>
          )}
          {(server.updates.agent || server.updates.telemt) && <UpdateChip>Update available</UpdateChip>}
          <ChevronRight className="ml-auto h-4 w-4 text-faint transition group-hover:text-foreground" />
        </div>
      </Card>
    </button>
  )
}

export function ServersScreen({ config, onChanged }: { config: AppConfig | null; onChanged: () => void }) {
  const { data: servers, error, loading, reload } = usePoll(api.servers, 10000)
  const [adding, setAdding] = useState(false)

  const max = config?.maxServers ?? 0
  const count = servers?.length ?? config?.serverCount ?? 0
  const full = max > 0 && count >= max

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-xl font-semibold">Servers</h1>
          <p className="text-[13px] text-muted">Each server's proxy reaches Telegram through this router.</p>
        </div>
        <div className="flex items-center justify-between gap-3">
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
          {max === 1 ? 'Small routers run one server, to keep memory free.' : "Each server's link uses router memory."} Remove one
          to add another.
        </Notice>
      )}
      {error && <ErrorBox>{error}</ErrorBox>}
      {loading && !servers && <Spinner />}

      {servers && servers.length === 0 && (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <TelegramMtprotoLogo size={48} />
          <h2 className="text-base font-semibold">No servers yet</h2>
          <p className="max-w-md text-[13px] text-muted">Add a Linux server over SSH. The login is used once, never stored.</p>
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
