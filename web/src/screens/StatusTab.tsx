import type { ReactNode } from 'react'
import type { Server } from '../lib/api'
import { eventLabel, formatBytes, formatCount, formatDuration, heldReason, relativeTime } from '../lib/format'
import { Badge, Card } from '../components/ui'

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card className="p-4">
      <h2 className="font-semibold">{title}</h2>
      <dl className="mt-2 divide-y divide-border">{children}</dl>
    </Card>
  )
}

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 py-1.5 text-[13px]">
      <dt className="text-muted">{label}</dt>
      <dd className="tabular min-w-0 break-words text-right">{children}</dd>
    </div>
  )
}

export function StatusTab({ server }: { server: Server }) {
  const { link, agent, sync } = server
  const t = agent?.telemt
  return (
    <div className="flex flex-col gap-4">
      {agent && (
        <p className="text-xs text-faint">
          Last report {relativeTime(agent.reportedAt)}
          {link.state === 'down' && ': the link is down, so these numbers are old.'}
        </p>
      )}
      <div className="grid gap-4 md:grid-cols-2">
        <Section title="Agent">
          <Item label="Version">{agent?.version || server.agentVersion || '–'}</Item>
          <Item label="Uptime">{agent ? formatDuration(agent.uptimeSec) : '–'}</Item>
          <Item label="Memory">{agent ? formatBytes(agent.rssBytes) : '–'}</Item>
          <Item label="Settings">
            {sync.healthy ? (
              <Badge tone="online">applied</Badge>
            ) : (
              <span className="text-status-warn">{sync.lastPushError || 'waiting to apply'}</span>
            )}
          </Item>
        </Section>

        <Section title="Proxy">
          <Item label="State">
            {!t ? (
              '–'
            ) : t.crashLoop ? (
              <Badge tone="danger">keeps crashing</Badge>
            ) : t.running ? (
              <Badge tone="online">running</Badge>
            ) : (
              <Badge tone="warn">stopped</Badge>
            )}
          </Item>
          <Item label="Version">{agent?.telemtVersion || server.telemtVersion || '–'}</Item>
          <Item label="Uptime">{t?.running ? formatDuration(t.uptimeSec) : '–'}</Item>
          <Item label="Restarts">{t ? formatCount(t.restarts) : '–'}</Item>
          {heldReason(t) && <Item label="Why stopped">{heldReason(t)}</Item>}
          {t?.lastExit && <Item label="Last exit">{t.lastExit}</Item>}
        </Section>

        <Section title="Link">
          <Item label="Connections">
            {link.up} of {link.total} up
          </Item>
          <Item label="Open streams">{formatCount(link.streams)}</Item>
          <Item label="Connections carried">{formatCount(link.relayed)}</Item>
          <Item label="To Telegram">{formatBytes(link.bytesToDc)}</Item>
          <Item label="From Telegram">{formatBytes(link.bytesFromDc)}</Item>
          <Item label="Failed dials">{formatCount(link.dialFailures)}</Item>
          <Item label="Refused destinations">{formatCount(link.violations)}</Item>
        </Section>

        <Section title="Capture">
          <Item label="Capture rules">
            {!agent ? '–' : agent.capture.rules ? <Badge tone="online">in place</Badge> : <Badge tone="danger">missing</Badge>}
          </Item>
          <Item label="Active">{agent ? formatCount(agent.capture.active) : '–'}</Item>
          <Item label="Rejected">{agent ? formatCount(agent.capture.rejected) : '–'}</Item>
          <Item label="Cover site">
            {!agent ? (
              server.coverDomain
            ) : !agent.cover.checkedAt ? (
              <span>
                {agent.cover.domain} <Badge tone="idle">not checked yet</Badge>
              </span>
            ) : agent.cover.ok ? (
              <span>
                {agent.cover.domain} <Badge tone="online">working</Badge>
              </span>
            ) : (
              <span className="text-status-danger">
                {agent.cover.domain}: {agent.cover.error || 'failing'}
              </span>
            )}
          </Item>
        </Section>
      </div>

      <Card className="overflow-hidden">
        <h2 className="px-4 pt-4 font-semibold">Link connections</h2>
        <div className="overflow-x-auto">
          <table className="mt-2 w-full text-left text-[13px]">
            <thead className="border-b border-border text-xs text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Connection</th>
                <th className="px-4 py-2 font-medium">State</th>
                <th className="px-4 py-2 font-medium">Since</th>
                <th className="px-4 py-2 font-medium">RTT</th>
                <th className="px-4 py-2 font-medium">Streams</th>
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
                  <td className="whitespace-nowrap px-4 py-2.5 text-muted">
                    {c.since ? relativeTime(c.since) : '–'}
                  </td>
                  <td className="tabular whitespace-nowrap px-4 py-2.5">{c.up ? `${c.rttMs} ms` : '–'}</td>
                  <td className="tabular px-4 py-2.5">{formatCount(c.streams)}</td>
                  <td className="min-w-[180px] px-4 py-2.5 text-xs text-status-danger">{c.lastError}</td>
                </tr>
              ))}
              {link.conns.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-4 py-3 text-muted">
                    No connections yet.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Card>

      <Card className="p-4">
        <h2 className="font-semibold">Recent events</h2>
        {server.events && server.events.length > 0 ? (
          <ul className="mt-2 divide-y divide-border">
            {server.events.map((e, i) => (
              <li key={i} className="flex flex-col gap-0.5 py-2 text-[13px] sm:flex-row sm:gap-4">
                <span className="w-32 shrink-0 text-xs text-faint" title={e.at}>
                  {relativeTime(e.at)}
                </span>
                <span className="font-medium">{eventLabel(e.kind)}</span>
                {e.detail && <span className="min-w-0 break-words text-muted">{e.detail}</span>}
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
