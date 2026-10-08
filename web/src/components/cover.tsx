import { CircleCheck, CircleX } from './icons'
import { ErrorBox } from './ui'
import type { CoverCheck } from '../lib/api'

/** What a cover site's test found, from the server. */
export function CheckResult({ check }: { check: CoverCheck }) {
  const items: [string, boolean][] = [
    ['TLS 1.3', check.tls13],
    ['X25519', check.x25519],
    ['HTTP/2', check.h2],
    ['Certificate', check.certOk],
  ]
  return (
    <div className="flex flex-col gap-2 rounded-md bg-card2 p-3 text-[13px]">
      <div className="flex flex-wrap gap-x-4 gap-y-1">
        {items.map(([label, ok]) => (
          <span key={label} className="inline-flex items-center gap-1.5">
            {ok ? (
              <CircleCheck className="h-4 w-4 text-status-online" />
            ) : (
              <CircleX className="h-4 w-4 text-status-danger" />
            )}
            <span>{label}</span>
          </span>
        ))}
      </div>
      {check.addrs.length > 0 && (
        <div className="text-xs text-muted">
          Resolves to <span className="font-mono">{check.addrs.join(', ')}</span>
          {check.latencyMs > 0 && `, ${check.latencyMs} ms from the server`}
        </div>
      )}
      {check.error && <ErrorBox>{check.error}</ErrorBox>}
    </div>
  )
}
