import { TrafficChart } from '../components/chart'
import { api, type LocalProxy } from '../lib/api'
import { eventLabel, formatBytes, formatCount, formatDuration, formatRate, relativeTime } from '../lib/format'
import { useNow, usePoll } from '../hooks/usePoll'
import { Badge, Card, cx, type Tone } from '../components/ui'
import { CHIP, Item, Section } from './StatusTab'

/** The proxy's state in a word or two, and its colour. */
export function proxyState(p: LocalProxy): { tone: Tone; text: string } {
  const t = p.proxy
  if (p.proxyStopped) return { tone: 'warn', text: 'stopped by you' }
  if (t.crashLoop) return { tone: 'danger', text: 'keeps crashing' }
  if (t.running) return { tone: 'online', text: 'running' }
  if (t.held) return { tone: 'warn', text: 'off' }
  return { tone: 'idle', text: 'starting' }
}

/** The one-line answer at the top, and the four things it rests on. */
function verdict(p: LocalProxy) {
  const a = p.address
  const chip = (label: string, value: string, tone: Tone) => ({ label, value, tone })
  const s = proxyState(p)
  const proxy = chip('Proxy', s.text, s.tone === 'idle' ? 'warn' : s.tone)
  const router = !a.reported
    ? chip('Router', 'waiting', 'idle')
    : !a.domestic
      ? chip('Router', 'no domestic link', 'danger')
      : chip('Router', `forwards port ${a.routerPort}`, 'online')
  const address = chip('Address', a.host ? a.host : 'not set', a.host ? 'online' : a.reported ? 'warn' : 'idle')
  const cover = chip('Cover site', !p.cover.checkedAt ? 'not checked yet' : p.cover.ok ? 'working' : 'failing', !p.cover.checkedAt ? 'idle' : p.cover.ok ? 'online' : 'danger')
  const warnings = [!!p.cover.checkedAt && !p.cover.ok, !!p.proxy.applyError, !a.host && a.reported].filter(Boolean).length
  const title =
    a.reported && !a.domestic
      ? "Phones can't reach this proxy"
      : proxy.tone !== 'online'
        ? "Users can't connect right now"
        : !a.reported
          ? 'Waiting for the router'
          : warnings
            ? `Working, with ${warnings === 1 ? 'one warning' : `${warnings} warnings`}`
            : 'Everything is working'
  return { title, chips: [proxy, router, address, cover] }
}

export function LocalStatus({ proxy: p }: { proxy: LocalProxy }) {
  const now = useNow(1000)
  const traffic = usePoll(() => api.traffic(1), 15000)
  const v = verdict(p)
  const t = p.proxy
  const a = p.address

  return (
    <div className="flex flex-col gap-4">
      <Card className="p-4">
        <h2 className="text-lg font-semibold">{v.title}</h2>
        <ul className="mt-3 grid grid-cols-2 gap-2 sm:flex sm:flex-wrap">
          {v.chips.map((c) => (
            <li key={c.label} className={cx('flex min-w-0 flex-col rounded-md px-3 py-2 text-[13px] sm:flex-row sm:items-center sm:gap-2 sm:py-1.5', CHIP[c.tone][0])}>
              <span className="flex items-center gap-2 text-muted">
                <span className={cx('hidden h-2 w-2 rounded-full sm:block', CHIP[c.tone][1])} />
                {c.label}
              </span>
              <b className={cx('truncate font-semibold', CHIP[c.tone][2])}>{c.value}</b>
            </li>
          ))}
        </ul>
      </Card>

      <Card className="p-4">
        <div className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between">
          <h2 className="font-semibold">Traffic, last 15 minutes</h2>
          <div className="flex flex-wrap gap-x-4 text-xs text-muted">
            <span className="inline-flex items-center gap-1.5">
              <span className="h-0.5 w-3 rounded bg-info" /> From Telegram <b className="tabular text-foreground">{formatRate(p.usage.rateDown)}</b>
            </span>
            <span className="inline-flex items-center gap-1.5">
              <span className="h-0.5 w-3 rounded bg-muted" /> To Telegram <b className="tabular text-foreground">{formatRate(p.usage.rateUp)}</b>
            </span>
          </div>
        </div>
        {traffic.data ? (
          <TrafficChart traffic={traffic.data} events={p.events} now={now} />
        ) : (
          <p className="mt-3 text-[13px] text-muted">{traffic.error ?? 'Loading…'}</p>
        )}
      </Card>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        <Section title="Proxy">
          <Item label="State">
            <Badge tone={proxyState(p).tone}>{proxyState(p).text}</Badge>
          </Item>
          <Item label="Version" hint={t.build || undefined}>
            {t.version || '–'}
          </Item>
          <Item label="Running for">{t.running ? formatDuration(t.uptimeSec) : 'not running'}</Item>
          <Item label="Restarts" tone={t.crashLoop ? 'danger' : undefined} hint={t.crashLoop && 'It restarts on its own and waits longer each time.'}>
            {formatCount(t.restarts)}
          </Item>
          <Item label="Memory">{t.running && t.rssBytes ? formatBytes(t.rssBytes) : '–'}</Item>
          <Item label="Connection limit">{formatCount(p.maxConns.value)}</Item>
          <Item label="Cover site">
            <span className="inline-flex flex-wrap items-center justify-end gap-1.5">
              {p.coverDomain}
              {!p.cover.checkedAt ? <Badge tone="idle">not checked yet</Badge> : p.cover.ok ? <Badge tone="online">working</Badge> : <Badge tone="danger">failing</Badge>}
            </span>
          </Item>
          {p.cover.checkedAt && !p.cover.ok && p.cover.error && <p className="py-2 text-xs text-bad-text">{p.cover.error}</p>}
          {!t.running && t.held && <Item label="Why stopped">{t.held}</Item>}
          {t.lastExit && <Item label="Last exit">{t.lastExit}</Item>}
        </Section>

        <Section title="Router" foot={a.reportedAt ? `Its script last changed something ${relativeTime(a.reportedAt, now)}.` : undefined}>
          <Item label="Domestic link" tone={a.reported && !a.domestic ? 'danger' : undefined}>
            {!a.reported ? '–' : a.domestic ? <Badge tone="online">in place</Badge> : <Badge tone="danger">missing</Badge>}
          </Item>
          <Item label="Port">{a.routerPort || '–'}</Item>
          <Item label="Domestic address">{a.wanAddress || '–'}</Item>
          <Item label="IP Cloud name" hint={!a.cloudName && a.reported ? 'IP Cloud is off, or this router has no name yet.' : undefined}>
            {a.cloudName || '–'}
          </Item>
          <Item label="Public address">{a.publicAddress || '–'}</Item>
          {a.cloudStatus && <Item label="IP Cloud">{a.cloudStatus}</Item>}
        </Section>

        <Section title="Plugin">
          <Item label="Version">{p.version}</Item>
          <Item label="Running for">{formatDuration(p.uptimeSec)}</Item>
          <Item label="Memory">{p.rssBytes ? formatBytes(p.rssBytes) : '–'}</Item>
        </Section>
      </div>

      <Card className="p-4">
        <h2 className="font-semibold">Recent events</h2>
        {p.events.length > 0 ? (
          <ul className="mt-2 divide-y divide-border">
            {p.events.map((e, i) => (
              <li key={i} className="flex flex-col py-2 text-[13px] sm:flex-row sm:gap-4">
                <span className="hidden w-32 shrink-0 text-xs text-faint sm:block" title={e.at}>
                  {relativeTime(e.at, now)}
                </span>
                <span className="font-medium">{eventLabel(e.kind)}</span>
                <span className="min-w-0 break-words text-xs text-muted sm:text-[13px]">
                  <span className="sm:hidden">
                    {relativeTime(e.at, now)}
                    {e.detail && ' · '}
                  </span>
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
