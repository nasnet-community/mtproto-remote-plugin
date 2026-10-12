import type { ReactNode } from 'react'
import { Info, TriangleAlert } from '../components/icons'
import { Button, cx } from '../components/ui'
import type { Server } from '../lib/api'
import { clockOrDay, relativeTime } from '../lib/format'
import type { Tab } from '../hooks/useRoute'

type Act = { label: string; primary?: boolean; run: () => void }
type Item = { tone: 'danger' | 'warn' | 'info'; title: string; detail: ReactNode; acts?: Act[] }

export interface AttentionActions {
  repair: () => void
  restart: () => void
  start: () => void
  remove: () => void
  changeCover: () => void
  open: (tab: Tab) => void
}

const sentence = (s: string) => {
  const t = s.charAt(0).toUpperCase() + s.slice(1)
  return /[.!?]$/.test(t) ? t : `${t}.`
}

/** What is wrong with a server, the likely cause, and one or two fixes, worst first. */
function items(s: Server, tab: Tab, cause: { text: string; at: string | null } | null, a: AttentionActions): Item[] {
  const out: Item[] = []
  const agent = s.agent
  const t = agent?.telemt
  if (!s.registered)
    return [
      {
        tone: 'danger',
        title: "This server's install never finished",
        detail: "It can't be repaired. Remove it, then add it again.",
        acts: [{ label: 'Remove server…', run: a.remove }],
      },
    ]
  if (s.link.state === 'down')
    // what the agent said last is old news while the link is down
    return [
      {
        tone: 'danger',
        title: "The router can't reach this server",
        detail: `${s.lastSeenAt ? `Last seen at ${clockOrDay(s.lastSeenAt)}, ${relativeTime(s.lastSeenAt)}. ` : ''}Check that the server is on and its address is on the domestic list, then run Repair.`,
        acts: [{ label: 'Repair', primary: true, run: a.repair }],
      },
    ]
  if (t?.crashLoop)
    out.push({
      tone: 'danger',
      title: 'The proxy keeps crashing',
      detail: cause
        ? `${cause.text} From the log${cause.at ? `, ${relativeTime(cause.at)}` : ''}.`
        : `${t.lastExit ? `It last stopped with "${t.lastExit}". ` : ''}The log may say why.`,
      acts: [{ label: 'Restart proxy', primary: true, run: a.restart }, ...(tab === 'logs' ? [] : [{ label: 'View logs', run: () => a.open('logs') }])],
    })
  if (agent && !agent.capture.rules)
    out.push({
      tone: 'danger',
      title: 'Telegram traffic rules are missing',
      detail: "The proxy can't reach Telegram without them. Repair puts them back.",
      acts: [{ label: 'Repair', run: a.repair }],
    })
  if (s.proxyStopped)
    out.push({
      tone: 'warn',
      title: 'You stopped the proxy',
      detail: 'It stays off, across restarts and repairs, until you start it.',
      acts: [{ label: 'Start proxy', primary: true, run: a.start }],
    })
  else if (t && !t.running && !t.crashLoop)
    out.push({ tone: 'warn', title: 'The proxy is not running', detail: t.held ? sentence(t.held) : t.lastExit ? sentence(t.lastExit) : 'The log may say why.' })
  if (agent?.cover.checkedAt && !agent.cover.ok)
    out.push({
      tone: 'warn',
      title: "The cover site isn't working",
      detail: `${agent.cover.domain}: ${agent.cover.error || 'the check failed'}. The proxy's disguise needs it.`,
      acts: [{ label: 'Change cover site…', run: a.changeCover }],
    })
  if (!s.sync.healthy && s.sync.lastPushError)
    out.push({ tone: 'warn', title: "Settings aren't applied yet", detail: sentence(s.sync.lastPushError) })
  if (s.link.state === 'degraded') {
    const down = s.link.conns.filter((c) => !c.up)
    out.push({
      tone: 'warn',
      title: `${s.link.up} of ${s.link.total} link connections are up`,
      detail: `${down
        .map((c) => `${c.name} ${/timeout/i.test(c.lastError) ? 'timed out' : 'went down'}${c.since ? ` ${relativeTime(c.since)}` : ''}`)
        .join(', ')}. Users still get through on the other ${s.link.up === 1 ? 'one' : 'ones'}.`,
      acts: tab === 'status' ? [] : [{ label: 'Open Status', run: () => a.open('status') }],
    })
  }
  if (!agent) out.push({ tone: 'info', title: "Waiting for the agent's first report", detail: 'It usually arrives within a few seconds.' })
  return out
}

const ICON = {
  danger: 'bg-bad-tint text-bad-text',
  warn: 'bg-warn-tint text-warn-text',
  info: 'bg-track text-muted',
}

/** One list above the tabs instead of stacked banners. */
export function Attention(props: { server: Server; tab: Tab; cause: { text: string; at: string | null } | null; actions: AttentionActions }) {
  const list = items(props.server, props.tab, props.cause, props.actions)
  if (!list.length) return null
  return (
    <section aria-label="Needs attention" className="flex flex-col gap-2">
      {list.map((it) => (
        <div key={it.title} className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4 shadow-sm sm:flex-row sm:items-center">
          <div className="flex min-w-0 flex-1 gap-3">
            <span className={cx('flex h-8 w-8 shrink-0 items-center justify-center rounded-full', ICON[it.tone])}>
              {it.tone === 'info' ? <Info className="h-4 w-4" /> : <TriangleAlert className="h-4 w-4" />}
            </span>
            <div className="min-w-0">
              <div className="font-semibold">{it.title}</div>
              <div className="text-[13px] text-muted">{it.detail}</div>
            </div>
          </div>
          {it.acts && it.acts.length > 0 && (
            <div className="grid grid-flow-col auto-cols-fr gap-2 sm:flex">
              {it.acts.map((x) => (
                <Button key={x.label} size="sm" variant={x.primary ? 'primary' : 'secondary'} onClick={x.run}>
                  {x.label}
                </Button>
              ))}
            </div>
          )}
        </div>
      ))}
    </section>
  )
}
