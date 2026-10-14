import { useEffect, useMemo, useRef, useState } from 'react'
import { ArrowDown, Download, Search } from '../components/icons'
import { api, type Server } from '../lib/api'
import { formatCount, parseLogLine } from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import { Button, Card, ErrorBox, Input, Segmented, Spinner, cx } from '../components/ui'

type Lines = 100 | 300 | 1000

const SIZES: [Lines, string][] = [
  [100, '100'],
  [300, '300'],
  [1000, '1,000 lines'],
]

/** How many lines in `next` came after the last one shown. */
function newer(shown: string[], next: string[]) {
  const last = shown[shown.length - 1]
  if (last === undefined) return next.length
  const i = next.lastIndexOf(last)
  return i < 0 ? next.length : next.length - 1 - i
}

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

function Marked({ text, find }: { text: string; find: RegExp | null }) {
  if (!find) return <>{text}</>
  return <>{text.split(find).map((p, i) => (i % 2 ? <mark key={i}>{p}</mark> : p))}</>
}

const TINT = { error: 'bg-bad-tint text-bad-text', warn: 'bg-warn-tint text-warn-text', '': '' }

/** The agent's recent log: newest at the bottom, following new lines until you scroll up. */
export function LogsTab({ server }: { server: Server }) {
  const [size, setSize] = useState<Lines>(300)
  const { data, error } = usePoll(() => api.logs(server.id, size), 5000, [server.id, size])
  const [shown, setShown] = useState<string[] | null>(null)
  const [follow, setFollow] = useState(true)
  const [query, setQuery] = useState('')
  const box = useRef<HTMLDivElement>(null)

  // while paused, the view stays put and new lines wait
  useEffect(() => {
    if (data && (follow || !shown)) setShown(data.lines)
  }, [data])
  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [shown, follow, query])

  const waiting = !follow && shown && data ? newer(shown, data.lines) : 0
  const resume = () => {
    setFollow(true)
    if (data) setShown(data.lines)
  }
  const onScroll = () => {
    const el = box.current!
    const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 12
    if (atEnd && !follow) resume()
    else if (!atEnd && follow) setFollow(false)
  }

  const find = query.trim() ? new RegExp(`(${escape(query.trim())})`, 'i') : null
  const rows = useMemo(() => {
    const all = (shown ?? []).map((line) => ({ line, ...parseLogLine(line) }))
    return find ? all.filter((r) => find.test(r.line)) : all
  }, [shown, query])

  const download = () => {
    const text = (data?.lines ?? shown ?? []).join('\n')
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([text + '\n'], { type: 'text/plain' }))
    a.download = `${server.name.replace(/[^\w.-]+/g, '-').toLowerCase()}-log-${new Date().toISOString().slice(0, 10)}.txt`
    a.click()
    setTimeout(() => URL.revokeObjectURL(a.href), 1000)
  }

  return (
    <Card className="flex flex-col gap-3 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <Segmented label="Lines to show" value={size} options={SIZES} onChange={setSize} />
        <div className="relative min-w-[180px] flex-1">
          <Search className="pointer-events-none absolute left-3 top-3.5 h-4 w-4 text-faint sm:top-2.5" />
          <Input type="search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search the log" aria-label="Search the log" className="pl-9" />
        </div>
        <Button size="sm" aria-pressed={follow} onClick={() => (follow ? setFollow(false) : resume())}>
          <span className={cx('h-2 w-2 rounded-full', follow ? 'bg-ok' : 'bg-warn')} />
          {follow ? 'Following new lines' : waiting ? `Paused · ${formatCount(waiting)} new` : 'Paused'}
        </Button>
        <Button size="sm" onClick={download} disabled={!data && !shown}>
          <Download className="h-3.5 w-3.5" /> Download
        </Button>
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
      {!shown && !error && <Spinner />}
      {shown && (
        <div className="relative">
          <div
            ref={box}
            onScroll={onScroll}
            role="log"
            aria-label="Log lines"
            tabIndex={0}
            className="max-h-[65vh] min-h-[160px] overflow-auto rounded-md border border-border bg-card2 py-2 font-mono text-[11.5px] leading-relaxed sm:text-xs"
          >
            {rows.map((r, i) => (
              <div key={i} className={cx('grid grid-cols-[auto_1fr] gap-x-3 px-3 sm:grid-cols-[auto_3.5rem_1fr] sm:gap-x-4', TINT[r.tone])}>
                <span className="text-faint">{r.at ? new Date(r.at).toTimeString().slice(0, 8) : ''}</span>
                <span className="hidden text-faint sm:block">{r.src}</span>
                <span className="min-w-0 whitespace-pre-wrap break-words">
                  <span className="text-faint sm:hidden">{r.src && `${r.src} `}</span>
                  <Marked text={r.msg} find={find} />
                </span>
              </div>
            ))}
            {rows.length === 0 && <div className="px-3 text-muted">{find ? 'No line matches.' : 'No log lines yet.'}</div>}
          </div>
          {!follow && (
            <Button size="sm" variant="primary" className="absolute bottom-3 right-3 shadow-lg" onClick={resume}>
              <ArrowDown className="h-3.5 w-3.5" /> {waiting ? `${formatCount(waiting)} new ${waiting === 1 ? 'line' : 'lines'}` : 'Back to newest'}
            </Button>
          )}
        </div>
      )}
      <div className="flex flex-col gap-1 text-xs text-faint sm:flex-row sm:justify-between">
        <span>Newest at the bottom. Scroll up to pause; new lines wait until you come back down.</span>
        {data && <span>{formatCount(data.lines.length)} lines, kept in the agent's memory</span>}
      </div>
    </Card>
  )
}
