import { useEffect, useMemo, useState, type ReactNode } from 'react'
import {
  CalendarPlus,
  Ellipsis,
  KeyRound,
  ListChecks,
  ListFilter,
  Pause,
  Pencil,
  Play,
  Plus,
  QrCode as QrCodeIcon,
  RotateCcw,
  Search,
  SearchX,
  Trash2,
  Users,
  X,
} from '../components/icons'
import { users as usersApi, type BulkAction, type Server, type User } from '../lib/api'
import {
  FILTERS,
  SORTS,
  barTone,
  clockOrDay,
  dayText,
  expiry,
  formatBytes,
  matchesFilter,
  nameList,
  plural,
  quotaPercent,
  quotaText,
  renewed,
  secondsAgo,
  sortUsers,
  userStatus,
  type Filter,
  type Sort,
  type StatusKey,
} from '../lib/format'
import { useNow, usePoll } from '../hooks/usePoll'
import {
  Badge,
  Bar,
  Button,
  Caret,
  Card,
  Chip,
  Confirm,
  ErrorBox,
  Input,
  Menu,
  Notice,
  Spinner,
  Switch,
  cx,
  useToast,
  type MenuItem,
  type Tone,
} from '../components/ui'
import { ShareDialog, UserDialog } from './UserDialogs'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

const PILL: Record<StatusKey, Tone> = {
  online: 'online',
  active: 'active',
  paused: 'muted',
  expired: 'danger',
  quota: 'warn',
  unknown: 'muted',
}

const EXPIRY_TEXT = { never: 'text-muted', normal: 'text-foreground', soon: 'font-medium text-warn-text', past: 'font-medium text-bad-text' }

const devices = (u: User) => ({
  main: u.devices === null ? '–' : u.maxIps ? `${u.devices} of ${u.maxIps}` : String(u.devices),
  sub: u.maxIps ? (u.devices === null ? `limit ${u.maxIps}` : 'devices now') : 'no limit',
  line: u.maxIps ? `${u.devices ?? '–'} of ${u.maxIps} devices` : 'No device limit',
})

/** "Updated 10 s ago", ticking. */
function Updated({ at }: { at: string | null }) {
  const now = useNow(1000)
  return at ? <span className="text-xs text-faint">Updated {secondsAgo(at, now)}</span> : null
}

function Tile({ label, short, value, unit, sub }: { label: string; short: string; value: ReactNode; unit?: [string, string]; sub: string }) {
  return (
    <Card className="min-w-0 px-1.5 py-2 sm:px-4 sm:py-3">
      <div className="truncate text-[10.5px] text-muted sm:text-xs">
        <span className="sm:hidden">{short}</span>
        <span className="hidden sm:inline">{label}</span>
      </div>
      <div className="tabular truncate sm:mt-0.5">
        <span className="text-[15px] font-semibold sm:text-xl">{value}</span>
        {unit && (
          <span className="text-[11px] text-muted sm:text-[13px]">
            <span className="sm:hidden">{unit[0]}</span>
            <span className="hidden sm:inline">{unit[1]}</span>
          </span>
        )}
      </div>
      <div className="hidden truncate text-xs text-faint sm:block">{sub}</div>
    </Card>
  )
}

type Modal = { kind: 'add' } | { kind: 'edit' | 'share' | 'rotate' | 'reset' | 'delete'; user: User } | { kind: 'bulk-delete'; names: string[] }

export function UsersTab({ server }: { server: Server }) {
  const toast = useToast()
  const { data, error, loading, reload } = usePoll(() => usersApi.list(server.id), 10000, [server.id])
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [sort, setSort] = useState<Sort>('name')
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [selecting, setSelecting] = useState(false)
  const [modal, setModal] = useState<Modal | null>(null)
  const [pending, setPending] = useState<string | null>(null)
  const down = server.link.state === 'down'
  const usage = server.usage

  const list = data ?? []
  const counts = useMemo(() => {
    const c = {} as Record<Filter, number>
    for (const [f] of FILTERS) c[f] = list.filter((u) => matchesFilter(u, f)).length
    return c
  }, [list])
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    const hit = (u: User) => !q || u.name.toLowerCase().includes(q) || u.note.toLowerCase().includes(q)
    return sortUsers(list.filter((u) => matchesFilter(u, filter) && hit(u)), sort)
  }, [list, filter, query, sort])
  // only what is on screen can be acted on
  const picked = shown.filter((u) => selected.has(u.name)).map((u) => u.name)

  // phones: toasts sit above the select mode's action bar
  useEffect(() => {
    document.documentElement.style.setProperty('--toast-lift', selecting ? '68px' : '0px')
    return () => void document.documentElement.style.removeProperty('--toast-lift')
  }, [selecting])

  const pick = (name: string, on: boolean) =>
    setSelected((s) => {
      const next = new Set(s)
      if (on) next.add(name)
      else next.delete(name)
      return next
    })
  const clear = () => {
    setSelected(new Set())
    setSelecting(false)
  }

  const run = async (name: string, fn: () => Promise<unknown>, done: string) => {
    setPending(name)
    try {
      await fn()
      toast(done)
      await reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setPending(null)
    }
  }
  const toggle = (u: User, on: boolean) => run(u.name, () => usersApi.update(server.id, u.name, { enabled: on }), `${u.name} ${on ? 'resumed' : 'paused'}`)
  const renew = (u: User) => {
    const next = renewed(u.expiresAt)
    return run(u.name, () => usersApi.update(server.id, u.name, { expiresAt: next }), `${u.name} renewed until ${next ? dayText(next) : 'never'}`)
  }

  const bulk = async (action: BulkAction, names = picked) => {
    try {
      const { changed } = await usersApi.bulk(server.id, action, names, action === 'renew' ? 30 : undefined)
      const done = { pause: 'paused', resume: 'resumed', renew: 'renewed', delete: 'deleted' }[action]
      const skipped = action === 'renew' && changed < names.length ? `; ${plural(names.length - changed, 'has', 'have')} no expiry` : ''
      toast(`${plural(changed, 'user')} ${done}${skipped}`)
      clear()
      await reload()
    } catch (e) {
      toast(message(e), 'error')
    }
  }
  const bulkActs: [BulkAction, string, ReactNode][] = [
    ['pause', 'Pause', <Pause key="p" className="h-4 w-4" />],
    ['resume', 'Resume', <Play key="r" className="h-4 w-4" />],
    ['renew', 'Renew +30 days', <CalendarPlus key="n" className="h-4 w-4" />],
    ['delete', 'Delete', <Trash2 key="d" className="h-4 w-4" />],
  ]
  const onBulk = (a: BulkAction) => (a === 'delete' ? setModal({ kind: 'bulk-delete', names: picked }) : void bulk(a))

  const actions = (u: User): MenuItem[] => [
    { label: 'Share link', icon: <QrCodeIcon />, onSelect: () => setModal({ kind: 'share', user: u }) },
    { label: 'Edit', icon: <Pencil />, hint: 'Limits, note and expiry', onSelect: () => setModal({ kind: 'edit', user: u }) },
    ...(u.expiresAt ? [{ label: 'Renew +30 days', icon: <CalendarPlus />, onSelect: () => void renew(u) }] : []),
    { label: 'Reset usage', icon: <RotateCcw />, onSelect: () => setModal({ kind: 'reset', user: u }) },
    { label: "Replace this user's link", icon: <KeyRound />, hint: 'The old link stops working', onSelect: () => setModal({ kind: 'rotate', user: u }) },
    { label: 'Delete user', icon: <Trash2 />, danger: true, divide: true, onSelect: () => setModal({ kind: 'delete', user: u }) },
  ]

  const menu = (u: User) => (
    <Menu label={`Actions for ${u.name}`} variant="ghost" items={actions(u)}>
      <Ellipsis className="h-4 w-4" />
    </Menu>
  )

  // the switch only pauses and resumes; an expired or used-up user gets the fix instead
  const control = (u: User, key: StatusKey) =>
    key === 'expired' || key === 'quota' ? null : (
      <Switch checked={u.enabled} disabled={pending === u.name} onChange={(on) => void toggle(u, on)} label={`${u.enabled ? 'Pause' : 'Resume'} ${u.name}`} />
    )
  const fix = (u: User, key: StatusKey, cls?: string) =>
    key === 'expired' ? (
      <Button size="sm" className={cls} busy={pending === u.name} onClick={() => void renew(u)} disabled={!u.expiresAt}>
        Renew +30 days
      </Button>
    ) : key === 'quota' ? (
      <Button size="sm" className={cls} onClick={() => setModal({ kind: 'reset', user: u })}>
        Reset usage
      </Button>
    ) : null

  const quota = (u: User) => {
    if (!u.quotaBytes) return <span className="text-muted">No quota</span>
    const pct = quotaPercent(u.up + u.down, u.quotaBytes)
    return (
      <div className="flex flex-col gap-1.5">
        <span className="tabular text-[12.5px]">{quotaText(u.up + u.down, u.quotaBytes)}</span>
        <Bar pct={pct} fill={barTone(pct)} />
      </div>
    )
  }

  const paused = counts.paused
  const cannot = [paused && `${paused} paused`, counts.expired && `${counts.expired} expired`, counts.quota && `${counts.quota} quota used`]
  const canConnect = list.filter((u) => u.state === 'active').length
  const sortLabel = SORTS.find(([s]) => s === sort)![1]
  const sortMenu = (
    <Menu
      label={`Sort: ${sortLabel}`}
      size="sm"
      items={SORTS.map(([s, label]) => ({ label, checked: s === sort, onSelect: () => setSort(s) }))}
    >
      <ListFilter className="h-3.5 w-3.5" /> Sort: {sortLabel} <Caret />
    </Menu>
  )
  const allPicked = shown.length > 0 && picked.length === shown.length

  return (
    <div className="flex flex-col gap-4">
      {down && (
        <Notice>
          This server is unreachable{server.lastSeenAt ? ` since ${clockOrDay(server.lastSeenAt)}` : ''}. What you change here applies when it's back.
        </Notice>
      )}
      <div className="grid grid-cols-4 gap-2 sm:gap-3">
        <Tile label="Can connect" short="Can connect" value={canConnect} unit={[`/${list.length}`, ` of ${list.length}`]} sub={down ? 'once the server is back' : cannot.filter(Boolean).join(' · ') || 'everyone'} />
        <Tile
          label="Online now"
          short="Online"
          value={usage.live ? usage.usersOnline : '–'}
          sub={usage.live ? `on ${plural(usage.devices, 'device')}` : down ? 'unknown while unreachable' : 'after an agent update'}
        />
        <Tile label="Today" short="Today" value={formatBytes(usage.todayUp + usage.todayDown)} sub={`↑ ${formatBytes(usage.todayUp)} · ↓ ${formatBytes(usage.todayDown)}`} />
        <Tile
          label="Last 30 days"
          short="30 days"
          value={formatBytes(usage.monthUp + usage.monthDown)}
          sub={`↑ ${formatBytes(usage.monthUp)} · ↓ ${formatBytes(usage.monthDown)}`}
        />
      </div>

      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div role="group" aria-label="Show" className="no-scrollbar -mx-4 flex gap-1.5 overflow-x-auto px-4 sm:mx-0 sm:flex-wrap sm:px-0">
            {FILTERS.map(([f, label]) => (
              <Chip key={f} on={filter === f} count={counts[f]} onClick={() => setFilter(f)}>
                {label}
              </Chip>
            ))}
          </div>
          <div className="hidden items-center gap-3 sm:flex">
            <Updated at={usage.updatedAt} />
            {sortMenu}
          </div>
        </div>
        <div className="flex gap-2">
          <div className="relative min-w-0 flex-1">
            <Search className="pointer-events-none absolute left-3 top-3.5 h-4 w-4 text-faint sm:top-2.5" />
            <Input type="search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search names and notes" className="pl-9" aria-label="Search users" />
          </div>
          <Button variant="primary" onClick={() => setModal({ kind: 'add' })}>
            <Plus className="h-4 w-4" />
            <span className="sm:hidden">Add</span>
            <span className="hidden sm:inline">Add user</span>
          </Button>
        </div>
        <div className="flex items-center justify-between gap-2 sm:hidden">
          <Updated at={usage.updatedAt} />
          <div className="ml-auto flex gap-2">
            {shown.length > 0 && (
              <Button size="sm" onClick={() => setSelecting(true)}>
                <ListChecks className="h-3.5 w-3.5" /> Select
              </Button>
            )}
            {sortMenu}
          </div>
        </div>
      </div>

      {error && <ErrorBox>{error}</ErrorBox>}
      {loading && !data && <Spinner />}

      {data && shown.length === 0 && (
        <Card className="flex flex-col items-center gap-2 px-6 py-10 text-center text-[13px] text-muted">
          {list.length === 0 ? <Users className="h-6 w-6" /> : <SearchX className="h-6 w-6" />}
          {list.length === 0 ? 'No users yet. Add one to get a proxy link.' : 'No user matches.'}
        </Card>
      )}

      {shown.length > 0 && (
        <>
          {/* a table on wide screens */}
          <Card className={cx('hidden overflow-hidden md:block', down && 'opacity-60')}>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[1000px] text-left text-[13px]">
                <thead className="border-b border-border text-xs text-muted">
                  <tr>
                    <th className="w-11 py-2.5 pl-4">
                      <input
                        type="checkbox"
                        className="h-4 w-4 align-middle"
                        aria-label="Select all users"
                        checked={allPicked}
                        ref={(el) => el && (el.indeterminate = picked.length > 0 && !allPicked)}
                        onChange={(e) => setSelected(new Set(e.target.checked ? shown.map((u) => u.name) : []))}
                      />
                    </th>
                    <th className="px-3 py-2.5 font-medium">User</th>
                    <th className="px-3 py-2.5 font-medium">Status</th>
                    <th className="px-3 py-2.5 font-medium">Devices</th>
                    <th className="px-3 py-2.5 font-medium">Quota</th>
                    <th className="px-3 py-2.5 font-medium">Traffic</th>
                    <th className="px-3 py-2.5 font-medium">Expires</th>
                    <th className="px-3 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {shown.map((u) => {
                    const st = userStatus(u, down)
                    const e = expiry(u.expiresAt)
                    const d = devices(u)
                    const on = selected.has(u.name)
                    return (
                      <tr key={u.name} className={cx('border-b border-border last:border-0', on && 'bg-track')}>
                        <td className="py-3 pl-4">
                          <input type="checkbox" className="h-4 w-4 align-middle" aria-label={`Select ${u.name}`} checked={on} onChange={(ev) => pick(u.name, ev.target.checked)} />
                        </td>
                        <td className="max-w-[200px] px-3 py-3">
                          <div className="truncate font-medium">{u.name}</div>
                          {u.note && <div className="truncate text-xs text-faint">{u.note}</div>}
                        </td>
                        <td className="px-3 py-3">
                          <div className="flex items-center gap-2.5">
                            {control(u, st.key)}
                            <div className="flex min-w-0 flex-col gap-0.5">
                              <Badge tone={PILL[st.key]}>{st.label}</Badge>
                              <span className="text-xs text-faint">{st.sub}</span>
                            </div>
                            {fix(u, st.key, 'ml-auto')}
                          </div>
                        </td>
                        <td className="tabular whitespace-nowrap px-3 py-3">
                          <div>{d.main}</div>
                          <div className="text-xs text-faint">{d.sub}</div>
                        </td>
                        <td className="w-[170px] px-3 py-3">{quota(u)}</td>
                        <td className="tabular whitespace-nowrap px-3 py-3">
                          <div>
                            {formatBytes(u.todayUp + u.todayDown)} <span className="text-xs text-faint">today</span>
                          </div>
                          <div className="text-xs text-faint">{formatBytes(u.monthUp + u.monthDown)} in 30 days</div>
                        </td>
                        <td className="whitespace-nowrap px-3 py-3" title={u.expiresAt ?? undefined}>
                          <div className={EXPIRY_TEXT[e.tone]}>{e.day}</div>
                          {e.rel && <div className="text-xs text-faint">{e.rel}</div>}
                        </td>
                        <td className="px-3 py-3">
                          <div className="flex justify-end gap-1">
                            <Button size="sm" variant="ghost" onClick={() => setModal({ kind: 'share', user: u })}>
                              <QrCodeIcon className="h-4 w-4" /> Share
                            </Button>
                            {menu(u)}
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </Card>

          {picked.length > 0 && (
            <div className="sticky bottom-3 z-10 hidden flex-wrap items-center gap-2 rounded-lg border border-border-strong bg-card2 px-4 py-2.5 shadow-lg md:flex">
              <span className="mr-1.5 text-[13px] font-semibold">{picked.length} selected</span>
              {bulkActs.map(([a, label, icon]) => (
                <Button key={a} size="sm" className={a === 'delete' ? 'text-bad-text' : undefined} onClick={() => onBulk(a)}>
                  {icon} {label}
                </Button>
              ))}
              <Button size="sm" variant="ghost" className="ml-auto text-muted" onClick={clear}>
                <X className="h-4 w-4" /> Clear
              </Button>
            </div>
          )}

          {/* cards on phones */}
          {!selecting ? (
            <ul className={cx('flex flex-col gap-3 md:hidden', down && 'opacity-60')}>
              {shown.map((u) => {
                const st = userStatus(u, down)
                const e = expiry(u.expiresAt)
                return (
                  <li key={u.name} className="flex flex-col gap-2.5 rounded-lg border border-border bg-card p-4">
                    <div className="flex items-center gap-3">
                      {control(u, st.key)}
                      <div className="min-w-0 flex-1">
                        <div className="truncate font-medium">{u.name}</div>
                        {u.note && <div className="truncate text-xs text-faint">{u.note}</div>}
                      </div>
                      <Badge tone={PILL[st.key]}>{st.label}</Badge>
                      <div className="-mr-2">{menu(u)}</div>
                    </div>
                    <div className="flex flex-wrap gap-x-4 text-xs text-muted">
                      <span>{devices(u).line}</span>
                      <span>{st.sub}</span>
                    </div>
                    {u.quotaBytes > 0 && quota(u)}
                    <div className="flex items-end gap-2">
                      <div className="min-w-0 flex-1 text-xs text-muted">
                        <div>
                          {formatBytes(u.todayUp + u.todayDown)} today · {formatBytes(u.monthUp + u.monthDown)} in 30 days
                        </div>
                        <div className={EXPIRY_TEXT[e.tone === 'normal' ? 'never' : e.tone]}>{e.line}</div>
                      </div>
                      {fix(u, st.key, 'px-2.5')}
                      <Button size="sm" onClick={() => setModal({ kind: 'share', user: u })}>
                        <QrCodeIcon className="h-4 w-4" /> Share
                      </Button>
                    </div>
                  </li>
                )
              })}
            </ul>
          ) : (
            <div className="md:hidden">
              <div className="fixed inset-x-0 top-0 z-30 flex h-14 items-center gap-2 border-b border-border bg-surface px-2">
                <Button variant="ghost" size="icon" aria-label="Leave select mode" onClick={clear}>
                  <X className="h-5 w-5" />
                </Button>
                <span className="flex-1 font-semibold">{picked.length} selected</span>
                <Button variant="ghost" size="sm" onClick={() => setSelected(new Set(allPicked ? [] : shown.map((u) => u.name)))}>
                  {allPicked ? 'Select none' : `Select all ${shown.length}`}
                </Button>
              </div>
              <p className="mb-2 text-xs text-muted">{server.name} · tap users to add or remove them</p>
              <ul className="flex flex-col gap-2 pb-24">
                {shown.map((u) => {
                  const st = userStatus(u, down)
                  const on = selected.has(u.name)
                  return (
                    <li key={u.name}>
                      <label className={cx('flex min-h-[60px] items-center gap-3 rounded-lg border p-3', on ? 'border-accent bg-track' : 'border-border bg-card')}>
                        <input type="checkbox" className="h-5 w-5 shrink-0" checked={on} onChange={(ev) => pick(u.name, ev.target.checked)} />
                        <span className="min-w-0 flex-1">
                          <span className="block font-medium">{u.name}</span>
                          <span className="block truncate text-xs text-muted">{[u.note, st.sub].filter(Boolean).join(' · ')}</span>
                        </span>
                        <Badge tone={PILL[st.key]}>{st.label}</Badge>
                      </label>
                    </li>
                  )
                })}
              </ul>
              <div className="pb-safe fixed inset-x-0 bottom-0 z-30 grid grid-cols-4 border-t border-border bg-surface pt-1">
                {bulkActs.map(([a, label, icon]) => (
                  <button
                    key={a}
                    type="button"
                    disabled={!picked.length}
                    onClick={() => onBulk(a)}
                    className={cx('flex min-h-[52px] flex-col items-center justify-center gap-1 px-1 text-xs font-medium disabled:opacity-40', a === 'delete' && 'text-bad-text')}
                  >
                    {icon}
                    {label}
                  </button>
                ))}
              </div>
            </div>
          )}

          <p className="text-xs text-faint">
            Showing {shown.length} of {list.length} · sorted by {sortLabel.toLowerCase()}
          </p>
        </>
      )}

      {modal?.kind === 'add' && (
        <UserDialog
          server={server}
          onClose={() => setModal(null)}
          onSaved={(created) => {
            void reload()
            setModal(created ? { kind: 'share', user: created } : null)
          }}
        />
      )}
      {modal?.kind === 'edit' && (
        <UserDialog
          server={server}
          user={modal.user}
          onClose={() => setModal(null)}
          onSaved={() => {
            void reload()
            setModal(null)
          }}
        />
      )}
      {modal?.kind === 'share' && <ShareDialog user={modal.user} onClose={() => setModal(null)} />}
      {modal?.kind === 'rotate' && (
        <Confirm
          title={`Replace ${modal.user.name}'s link?`}
          action="Replace link"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            const updated = await usersApi.rotateSecret(server.id, modal.user.name)
            toast(`${modal.user.name} has a new link`)
            void reload()
            setTimeout(() => setModal({ kind: 'share', user: updated }))
          }}
        >
          <p>Their current link stops working. You'll share the new link with them.</p>
        </Confirm>
      )}
      {modal?.kind === 'reset' && (
        <Confirm
          title={`Reset ${modal.user.name}'s usage?`}
          action="Reset usage"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.resetUsage(server.id, modal.user.name)
            toast(`${modal.user.name}'s usage was reset`)
            void reload()
          }}
        >
          <p>Their upload and download counters start again from zero, which also lifts a used-up quota.</p>
        </Confirm>
      )}
      {modal?.kind === 'delete' && (
        <Confirm
          danger
          title={`Delete ${modal.user.name}?`}
          action="Delete user"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.remove(server.id, modal.user.name)
            toast(`${modal.user.name} deleted`)
            void reload()
          }}
        >
          <p>Their link stops working and the user is removed from the {__LOCAL__ ? 'proxy' : 'server'}.</p>
        </Confirm>
      )}
      {modal?.kind === 'bulk-delete' && (
        <Confirm
          danger
          title={`Delete ${plural(modal.names.length, 'user')}?`}
          action={`Delete ${plural(modal.names.length, 'user')}`}
          onClose={() => setModal(null)}
          onConfirm={() => bulk('delete', modal.names)}
        >
          <p>
            {nameList(modal.names, 6)} {modal.names.length === 1 ? 'loses their link and is' : 'lose their links and are'} removed from the{' '}
            {__LOCAL__ ? 'proxy' : 'server'}.
          </p>
        </Confirm>
      )}
    </div>
  )
}
