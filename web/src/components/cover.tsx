import { Check, CircleCheck, CircleX, X } from './icons'
import { cx } from './ui'
import type { CoverCheck } from '../lib/api'

/** What a cover site's test found, from the server. */
export function CheckResult({ check, domain }: { check: CoverCheck; domain?: string }) {
  const items: [string, boolean][] = [
    ['TLS 1.3', check.tls13],
    ['X25519', check.x25519],
    ['HTTP/2', check.h2],
    ['Certificate', check.certOk],
  ]
  return (
    <div role="status" className="flex flex-col gap-2 rounded-md border border-border bg-card2 p-3 text-[13px]">
      <span className={cx('flex items-center gap-2 font-medium', check.ok ? 'text-ok-text' : 'text-bad-text')}>
        {check.ok ? <CircleCheck className="h-4 w-4" /> : <CircleX className="h-4 w-4" />}
        {check.ok ? 'Works as a cover site, tested from the server' : `${domain ?? 'This site'} doesn't work as a cover site`}
      </span>
      <div className="flex flex-wrap gap-x-4 gap-y-1">
        {items.map(([label, ok]) => (
          <span key={label} className="inline-flex items-center gap-1.5">
            {ok ? <Check className="h-4 w-4 text-ok-text" /> : <X className="h-4 w-4 text-bad-text" />}
            <span>{label}</span>
            <span className="sr-only">{ok ? 'passed' : 'failed'}</span>
          </span>
        ))}
      </div>
      {check.addrs.length > 0 && (
        <div className="text-xs text-muted">
          Resolves to <span className="font-mono">{check.addrs.join(', ')}</span>
          {check.latencyMs > 0 && `, ${check.latencyMs} ms from the server`}.
        </div>
      )}
      {check.error && <div className="text-xs text-bad-text">{check.error}</div>}
    </div>
  )
}
