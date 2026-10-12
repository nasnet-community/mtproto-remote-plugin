import type { ServerEvent, Traffic } from '../lib/api'
import { clockOrDay, formatRate } from '../lib/format'

const WINDOW = 15 * 60e3
const W = 600
const H = 140

/** The smallest of 1, 2 or 5 times a power of ten that is at least `v`. */
export function niceMax(v: number) {
  const top = Math.max(v, 100_000)
  const p = 10 ** Math.floor(Math.log10(top))
  return ([1, 2, 5, 10].find((m) => m * p >= top) ?? 10) * p
}

/** The last 15 minutes of a server's traffic, both ways, with the moment the proxy stopped. */
export function TrafficChart({ traffic, events, now }: { traffic: Traffic; events?: ServerEvent[]; now: number }) {
  const start = now - WINDOW
  const s = traffic.samples.filter((p) => Date.parse(p.at) >= start - traffic.stepSec * 1000)
  const top = niceMax(Math.max(0, ...s.map((x) => Math.max(x.up, x.down))))
  const x = (at: string) => Math.max(0, Math.min(W, ((Date.parse(at) - start) / WINDOW) * W))
  const y = (v: number) => H - (v / top) * H
  const line = (k: 'up' | 'down') => s.map((p, i) => `${i ? 'L' : 'M'}${x(p.at).toFixed(1)},${y(p[k]).toFixed(1)}`).join('')
  const area = s.length ? `${line('down')}L${x(s[s.length - 1].at).toFixed(1)},${H}L${x(s[0].at).toFixed(1)},${H}Z` : ''
  const stop = events?.find((e) => e.kind === 'telemt_down' && Date.parse(e.at) > start)
  const stopX = stop ? (x(stop.at) / W) * 100 : null
  const peak = Math.max(0, ...s.map((p) => p.down))
  const last = s[s.length - 1]
  const label = last
    ? `From Telegram ${formatRate(last.down)} now, at most ${formatRate(peak)} in the last 15 minutes. To Telegram ${formatRate(last.up)} now.`
    : 'No traffic samples yet.'

  return (
    <div role="img" aria-label={label} className="relative mt-3 pl-14">
      <div className="absolute inset-y-0 left-0 flex w-12 flex-col justify-between pb-6 text-right text-[11px] text-faint">
        <span className="-mt-1.5">{formatRate(top).replace('.0 ', ' ')}</span>
        <span>{formatRate(top / 2).replace('.0 ', ' ')}</span>
        <span className="-mb-1.5">0</span>
      </div>
      <div className="relative h-[140px]">
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="absolute inset-0 h-full w-full overflow-visible" aria-hidden="true">
          {[0, H / 2, H].map((g) => (
            <line key={g} x1={0} x2={W} y1={g} y2={g} stroke="var(--border)" strokeWidth={1} vectorEffect="non-scaling-stroke" />
          ))}
          {s.length > 1 && (
            <>
              <path d={area} fill="var(--info)" opacity={0.15} />
              <path d={line('down')} fill="none" stroke="var(--info)" strokeWidth={2} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
              <path d={line('up')} fill="none" stroke="var(--muted)" strokeWidth={1.5} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
            </>
          )}
          {stopX !== null && (
            <line x1={(stopX / 100) * W} x2={(stopX / 100) * W} y1={0} y2={H} stroke="var(--bad)" strokeWidth={1.5} strokeDasharray="4 4" vectorEffect="non-scaling-stroke" />
          )}
        </svg>
        {stop && stopX !== null && (
          <span className="absolute top-1 whitespace-nowrap text-[11px] text-bad-text" style={stopX > 70 ? { right: `${100 - stopX}%`, paddingRight: 6 } : { left: `${stopX}%`, paddingLeft: 6 }}>
            Proxy stopped {clockOrDay(stop.at, now)}
          </span>
        )}
        {s.length < 2 && (
          <span className="absolute inset-0 flex items-center justify-center text-[13px] text-muted">
            No samples yet. The router reads traffic every 15 seconds.
          </span>
        )}
      </div>
      <div className="relative mt-1.5 h-4 text-[11px] text-faint">
        <span className="absolute left-0">15 min ago</span>
        <span className="absolute left-1/3 -translate-x-1/2">10 min</span>
        <span className="absolute left-2/3 -translate-x-1/2">5 min</span>
        <span className="absolute right-0">now</span>
      </div>
    </div>
  )
}
