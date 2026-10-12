import type { ReactNode } from 'react'
import { TrafficChart } from '../components/chart'
import { api, type Server } from '../lib/api'
import { eventLabel, formatBytes, formatCount, formatDuration, formatRate, heldReason, relativeTime, secondsAgo } from '../lib/format'
import { useNow, usePoll } from '../hooks/usePoll'
import { Badge, Card, cx, type Tone } from '../components/ui'

function Section({ title, children, foot }: { title: string; children: ReactNode; foot?: string }) {
  return (
    <Card className="p-4">
      <h2 className="font-semibold">{title}</h2>
      <dl className="mt-2 divide-y divide-border">{children}</dl>
      {foot && <p className="mt-2 text-xs text-faint">{foot}</p>}
    </Card>
  )
}

const TEXT: Partial<Record<Tone, string>> = { danger: 'font-medium text-bad-text', warn: 'font-medium text-warn-text' }

/** A row; a bad number gets its tone and a hint that says what it means. */
function Item({ label, children, tone, hint }: { label: string; children: ReactNode; tone?: Tone; hint?: string | false }) {
  return (
    <div className="py-2 text-[13px]">
      <div className="flex items-baseline justify-between gap-3">
        <dt className="text-muted">{label}</dt>
        <dd className={cx('tabular min-w-0 break-words text-right', tone && TEXT[tone])}>{children}</dd>
      </div>
      {hint && <p className="mt-0.5 text-xs text-faint">{hint}</p>}
    </div>
  )
}

const Avail = ({ v }: { v: string }) => (
  <span className="ml-1.5 whitespace-nowrap rounded-full border border-border-strong px-1.5 py-px text-[11px] text-foreground">{v ? `${v} available` : 'update available'}</span>
)

/** The one-line answer at the top, and the four things it rests on. */
function verdict(s: Server) {
  const a = s.agent
  const t = a?.telemt
  const chip = (label: string, value: string, tone: Tone) => ({ label, value, tone })
  const proxy = !t
    ? chip('Proxy', '–', 'idle')
    : s.proxyStopped
      ? chip('Proxy', 'stopped by you', 'warn')
      : t.crashLoop
        ? chip('Proxy', 'keeps crashing', 'danger')
        : t.running
          ? chip('Proxy', 'running', 'online')
          : chip('Proxy', 'not running', 'warn')
  const l = s.link
  const link = chip('Link', l.state === 'down' ? 'down' : `${l.up} of ${l.total} up`, l.state === 'up' ? 'online' : l.state === 'down' ? 'danger' : 'warn')
  const rules = chip('Telegram traffic rules', !a ? '–' : a.capture.rules ? 'in place' : 'missing', !a ? 'idle' : a.capture.rules ? 'online' : 'danger')
  const cover = chip(
    'Cover site',
    !a ? '–' : !a.cover.checkedAt ? 'not checked yet' : a.cover.ok ? 'working' : 'failing',
    !a || !a.cover.checkedAt ? 'idle' : a.cover.ok ? 'online' : 'danger',
  )
  const warnings = [l.state === 'degraded', !!a?.cover.checkedAt && !a.cover.ok, !s.sync.healthy && !!s.sync.lastPushError].filter(Boolean).length
  const title =
    l.state === 'down'
      ? "The router can't reach this server"
      : proxy.tone === 'danger' || proxy.tone === 'warn' || rules.tone === 'danger'
        ? "Users can't connect right now"
        : !a
          ? "Waiting for the agent's first report"
          : warnings
            ? `Working, with ${warnings === 1 ? 'one warning' : `${warnings} warnings`}`
            : 'Everything is working'
  return { title, chips: [proxy, link, rules, cover] }
}

const CHIP: Record<Tone, [string, string, string]> = {
  online: ['bg-ok-tint', 'bg-ok', 'text-ok-text'],
  warn: ['bg-warn-tint', 'bg-warn', 'text-warn-text'],
  danger: ['bg-bad-tint', 'bg-bad', 'text-bad-text'],
  idle: ['bg-track', 'bg-faint', 'text-foreground'],
  active: ['bg-track', 'bg-faint', 'text-foreground'],
  muted: ['bg-track', 'bg-faint', 'text-foreground'],
}

export function StatusTab({ server }: { server: Server }) {
  const { link, agent, sync, updates } = server
  const t = agent?.telemt
  const now = useNow(1000)
  const traffic = usePoll(() => api.traffic(server.id), 15000, [server.id])
  const v = verdict(server)
  const down = link.state === 'down'
  const held = heldReason(t)

  return (
    <div className="flex flex-col gap-4">
      <Card className="p-4">
        <div className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between">
          <h2 className="text-lg font-semibold">{v.title}</h2>
          {agent && (
            <span className="text-xs text-faint">
              Last report {secondsAgo(agent.reportedAt, now)}
              {down && '; the link is down, so these numbers are old'}
            </span>
          )}
        </div>
        <ul className="mt-3 grid grid-cols-2 gap-2 sm:flex sm:flex-wrap">
          {v.chips.map((c) => (
            <li key={c.label} className={cx('flex flex-col rounded-md px-3 py-2 text-[13px] sm:flex-row sm:items-center sm:gap-2 sm:py-1.5', CHIP[c.tone][0])}>
              <span className="flex items-center gap-2 text-muted">
                <span className={cx('hidden h-2 w-2 rounded-full sm:block', CHIP[c.tone][1])} />
                {c.label}
              </span>
              <b className={cx('font-semibold', CHIP[c.tone][2])}>{c.value}</b>
            </li>
          ))}
        </ul>
      </Card>

      <Card className="p-4">
        <div className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between">
          <h2 className="font-semibold">Traffic, last 15 minutes</h2>
          <div className="flex flex-wrap gap-x-4 text-xs text-muted">
            <span className="inline-flex items-center gap-1.5">
              <span className="h-0.5 w-3 rounded bg-info" /> From Telegram <b className="tabular text-foreground">{formatRate(server.usage.rateDown)}</b>
            </span>
            <span className="inline-flex items-center gap-1.5">
              <span className="h-0.5 w-3 rounded bg-muted" /> To Telegram <b className="tabular text-foreground">{formatRate(server.usage.rateUp)}</b>
            </span>
          </div>
        </div>
        {traffic.data ? (
          <TrafficChart traffic={traffic.data} events={server.events} now={now} />
        ) : (
          <p className="mt-3 text-[13px] text-muted">{traffic.error ?? 'Loading…'}</p>
        )}
      </Card>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        <Section title="Agent">
          <Item label="Version">
            {agent?.version || server.agentVersion || '–'}
            {updates.agent && <Avail v={updates.agentVersion} />}
          </Item>
          <Item label="Running for">{agent ? formatDuration(agent.uptimeSec) : '–'}</Item>
          <Item label="Memory">{agent ? formatBytes(agent.rssBytes) : '–'}</Item>
          <Item label="Settings" tone={sync.healthy ? undefined : 'warn'}>
            {sync.healthy ? <Badge tone="online">applied</Badge> : sync.lastPushError || 'waiting to apply'}
          </Item>
        </Section>

        <Section title="Proxy">
          <Item label="State">
            {!t ? (
              '–'
            ) : server.proxyStopped ? (
              <Badge tone="warn">stopped by you</Badge>
            ) : t.crashLoop ? (
              <Badge tone="danger">keeps crashing</Badge>
            ) : t.running ? (
              <Badge tone="online">running</Badge>
            ) : (
              <Badge tone="warn">stopped</Badge>
            )}
          </Item>
          <Item label="Version">
            {agent?.telemtVersion || server.telemtVersion || '–'}
            {updates.telemt && <Avail v={updates.telemtVersion} />}
          </Item>
          <Item label="Running for">{!t ? '–' : t.running ? formatDuration(t.uptimeSec) : 'not running'}</Item>
          <Item label="Restarts" tone={t?.crashLoop ? 'danger' : undefined} hint={t?.crashLoop && 'It restarts on its own and waits longer each time.'}>
            {t ? formatCount(t.restarts) : '–'}
          </Item>
          {held && <Item label="Why stopped">{held}</Item>}
          {t?.lastExit && <Item label="Last exit">{t.lastExit}</Item>}
        </Section>

        <Section title="Link to this router" foot="Totals count from the plugin's last start.">
          <Item
            label="Link connections"
            tone={link.state === 'up' ? undefined : link.state === 'down' ? 'danger' : 'warn'}
            hint={link.state === 'degraded' ? 'Users still get through on the other connection.' : link.state === 'down' && "Users can't connect until it's back."}
          >
            {link.up} of {link.total} up
          </Item>
          <Item label="Telegram connections now">{formatCount(link.streams)}</Item>
          <Item label="Connections carried">{formatCount(link.relayed)}</Item>
          <Item label="To Telegram">{formatBytes(link.bytesToDc)}</Item>
          <Item label="From Telegram">{formatBytes(link.bytesFromDc)}</Item>
          <Item
            label="Couldn't reach Telegram"
            tone={link.dialFailures ? 'warn' : undefined}
            hint={link.dialFailures > 0 && 'The router could not open a connection to Telegram. A few now and then is normal.'}
          >
            {formatCount(link.dialFailures)}
          </Item>
          <Item
            label="Blocked non-Telegram addresses"
            tone={link.violations ? 'warn' : undefined}
            hint={link.violations > 0 && 'Something on the server tried to reach a non-Telegram address; the router refused it.'}
          >
            {formatCount(link.violations)}
          </Item>
        </Section>

        <Section title="Telegram traffic rules">
          <Item
            label="Rules on the server"
            hint={!!agent && !agent.capture.rules && "The proxy can't reach Telegram without them. Repair puts them back."}
          >
            {!agent ? '–' : agent.capture.rules ? <Badge tone="online">in place</Badge> : <Badge tone="danger">missing</Badge>}
          </Item>
          <Item label="Sent to the router now">{agent ? formatCount(agent.capture.active) : '–'}</Item>
          <Item label="Refused on the server" hint={!!agent?.capture.rejected && "Connections the proxy tried that weren't for Telegram."}>
            {agent ? formatCount(agent.capture.rejected) : '–'}
          </Item>
          <Item label="Cover site">
            {!agent ? (
              server.coverDomain
            ) : (
              <span className="inline-flex flex-wrap items-center justify-end gap-1.5">
                {agent.cover.domain}
                {!agent.cover.checkedAt ? (
                  <Badge tone="idle">not checked yet</Badge>
                ) : agent.cover.ok ? (
                  <Badge tone="online">working</Badge>
                ) : (
                  <Badge tone="danger">failing</Badge>
                )}
              </span>
            )}
          </Item>
          {agent?.cover.checkedAt && !agent.cover.ok && agent.cover.error && (
            <p className="py-2 text-xs text-bad-text">{agent.cover.error}</p>
          )}
        </Section>
      </div>

      <Card className="overflow-hidden">
        <h2 className="px-4 pt-4 font-semibold">Link connections</h2>
        <table className="mt-2 hidden w-full text-left text-[13px] sm:table">
          <thead className="border-b border-border text-xs text-muted">
            <tr>
              <th className="px-4 py-2 font-medium">Connection</th>
              <th className="px-4 py-2 font-medium">State</th>
              <th className="px-4 py-2 font-medium">Since</th>
              <th className="px-4 py-2 font-medium">Round trip</th>
              <th className="px-4 py-2 font-medium">Connections</th>
              <th className="px-4 py-2 font-medium">Last error</th>
            </tr>
          </thead>
          <tbody>
            {link.conns.map((c) => (
              <tr key={c.name} className="border-b border-border last:border-0">
                <td className="px-4 py-2.5 font-mono text-xs">{c.name}</td>
                <td className="px-4 py-2.5">
                  <Badge tone={c.up ? 'online' : 'danger'}>{c.up ? 'up' : 'down'}</Badge>
                </td>
                <td className="whitespace-nowrap px-4 py-2.5 text-muted">{c.since ? relativeTime(c.since, now) : '–'}</td>
                <td className="tabular whitespace-nowrap px-4 py-2.5">{c.up ? `${c.rttMs} ms` : '–'}</td>
                <td className="tabular px-4 py-2.5">{formatCount(c.streams)}</td>
                <td className="min-w-[180px] px-4 py-2.5 text-xs text-bad-text">{c.lastError}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <ul className="mt-1 divide-y divide-border sm:hidden">
          {link.conns.map((c) => (
            <li key={c.name} className="px-4 py-3 text-[13px]">
              <div className="flex items-center justify-between">
                <span className="font-mono text-xs">{c.name}</span>
                <Badge tone={c.up ? 'online' : 'danger'}>{c.up ? 'up' : 'down'}</Badge>
              </div>
              <div className="mt-0.5 text-xs text-muted">
                {[c.since && `Since ${relativeTime(c.since, now)}`, c.up && `round trip ${c.rttMs} ms`, c.up && `${formatCount(c.streams)} connections`]
                  .filter(Boolean)
                  .join(' · ')}
              </div>
              {c.lastError && <div className="mt-0.5 break-words text-xs text-bad-text">{c.lastError}</div>}
            </li>
          ))}
        </ul>
        {link.conns.length === 0 && <p className="px-4 pb-4 pt-2 text-[13px] text-muted">No connections yet.</p>}
        <div className="h-2" />
      </Card>

      <Card className="p-4">
        <h2 className="font-semibold">Recent events</h2>
        {server.events && server.events.length > 0 ? (
          <ul className="mt-2 divide-y divide-border">
            {server.events.map((e, i) => (
              <li key={i} className="flex flex-col py-2 text-[13px] sm:flex-row sm:gap-4">
                <span className="hidden w-32 shrink-0 text-xs text-faint sm:block" title={e.at}>
                  {relativeTime(e.at, now)}
                </span>
                <span className="font-medium">{eventLabel(e.kind)}</span>
                <span className="min-w-0 break-words text-xs text-muted sm:text-[13px]">
                  <span className="sm:hidden">{relativeTime(e.at, now)}{e.detail && ' · '}</span>
                  {e.detail}
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="mt-2 text-[13px] text-muted">No events yet.</p>
        )}
      </Card>
    </div>
  )
}
