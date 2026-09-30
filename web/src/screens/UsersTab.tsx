import { useMemo, useState } from 'react'
import {
  Activity,
  Ellipsis,
  KeyRound,
  Plus,
  QrCode as QrCodeIcon,
  RotateCcw,
  Search,
  SearchX,
  SlidersHorizontal,
  Trash2,
  Users,
} from '../components/icons'
import { users as usersApi, type Server, type TelemtUser, type UserLimits } from '../lib/api'
import {
  GB,
  MBIT,
  USERNAME,
  formatBytes,
  limitText,
  parseLimit,
  relativeTime,
  toWebLink,
} from '../lib/format'
import { usePoll } from '../hooks/usePoll'
import {
  Badge,
  Button,
  Card,
  CopyButton,
  Dialog,
  ErrorBox,
  Field,
  Input,
  Menu,
  QrCode,
  Spinner,
  Switch,
  cx,
  useToast,
} from '../components/ui'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

function expired(user: TelemtUser) {
  return !!user.expiration_rfc3339 && new Date(user.expiration_rfc3339).getTime() < Date.now()
}

// The proxy does not report how much of a quota is used (its counter is separate
// from total traffic and resets independently), so show the two side by side.
function Traffic({ user }: { user: TelemtUser }) {
  return (
    <div className="flex flex-col">
      <span className="tabular">{formatBytes(user.total_octets)}</span>
      {user.data_quota_bytes !== null && (
        <span className="text-xs text-faint">quota {formatBytes(user.data_quota_bytes)}</span>
      )}
    </div>
  )
}

function UserState({ user }: { user: TelemtUser }) {
  if (!user.enabled) return <Badge tone="idle">disabled</Badge>
  if (expired(user)) return <Badge tone="danger">expired</Badge>
  if (user.current_connections > 0) return <Badge tone="online">online</Badge>
  return <Badge tone="idle">idle</Badge>
}

// --- Add / edit ------------------------------------------------------------

interface LimitForm {
  username: string
  maxConns: string
  maxIps: string
  quotaGb: string
  expires: string
  downMbit: string
  upMbit: string
}

function toForm(user?: TelemtUser): LimitForm {
  return {
    username: user?.username ?? '',
    maxConns: limitText(user?.max_tcp_conns ?? null),
    maxIps: limitText(user?.max_unique_ips ?? null),
    quotaGb: limitText(user?.data_quota_bytes ?? null, GB),
    expires: user?.expiration_rfc3339 ? user.expiration_rfc3339.slice(0, 10) : '',
    downMbit: limitText(user?.rate_limit_down_bps ?? null, MBIT),
    upMbit: limitText(user?.rate_limit_up_bps ?? null, MBIT),
  }
}

function toLimits(f: LimitForm): UserLimits | string {
  const fields: [keyof UserLimits, string, number, string][] = [
    ['max_tcp_conns', f.maxConns, 1, 'Connections'],
    ['max_unique_ips', f.maxIps, 1, 'Devices'],
    ['data_quota_bytes', f.quotaGb, GB, 'Traffic quota'],
    ['rate_limit_down_bps', f.downMbit, MBIT, 'Download speed'],
    ['rate_limit_up_bps', f.upMbit, MBIT, 'Upload speed'],
  ]
  const limits: UserLimits = {}
  for (const [key, text, scale, label] of fields) {
    const value = parseLimit(text, scale)
    if (value === 'invalid') return `${label} must be a positive number, or empty for no limit.`
    ;(limits as Record<string, number | null>)[key] = value
  }
  // The date picker gives a day; the user stays valid through its end (UTC).
  limits.expiration_rfc3339 = f.expires ? `${f.expires}T23:59:59Z` : null
  return limits
}

function UserDialog({
  server,
  user,
  onClose,
  onSaved,
}: {
  server: Server
  user?: TelemtUser
  onClose: () => void
  onSaved: (created?: TelemtUser) => void
}) {
  const toast = useToast()
  const [form, setForm] = useState(() => toForm(user))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const set = (k: keyof LimitForm) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: e.target.value }))

  const save = async () => {
    setError(null)
    const limits = toLimits(form)
    if (typeof limits === 'string') return setError(limits)
    const name = form.username.trim()
    if (!user && !USERNAME.test(name)) {
      return setError("Use 1–64 letters, digits, '.', '_' or '-' for the name.")
    }
    setBusy(true)
    try {
      if (user) {
        await usersApi.update(server.id, user.username, limits)
        toast(`${user.username} updated`)
        onSaved()
      } else {
        // A new user has no limits to clear, so leave out the nulls.
        const set = Object.fromEntries(Object.entries(limits).filter(([, v]) => v !== null))
        const res = await usersApi.create(server.id, name, set)
        toast(`${name} added`)
        onSaved(res.user)
      }
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }

  const numeric = (label: string, key: keyof LimitForm, unit: string, hint?: string) => (
    <Field label={label} hint={hint}>
      {(id) => (
        <div className="relative">
          <Input
            id={id}
            inputMode="decimal"
            value={form[key]}
            onChange={set(key)}
            placeholder="No limit"
            className="pr-14"
          />
          <span className="pointer-events-none absolute right-3 top-2 text-xs text-faint">
            {unit}
          </span>
        </div>
      )}
    </Field>
  )

  return (
    <Dialog
      open
      onClose={onClose}
      title={user ? `Limits for ${user.username}` : 'Add a user'}
      description="Leave a field empty for no limit."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={busy} onClick={save}>
            {user ? 'Save' : 'Add user'}
          </Button>
        </>
      }
    >
      <form
        className="grid grid-cols-2 gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          void save()
        }}
      >
        {!user && (
          <Field
            label="Name"
            hint="Letters, digits, '.', '_' or '-'. A secret is generated for it."
            className="col-span-2"
          >
            {(id) => (
              <Input
                id={id}
                value={form.username}
                onChange={set('username')}
                maxLength={64}
                spellCheck={false}
                autoCapitalize="off"
                placeholder="alice"
              />
            )}
          </Field>
        )}
        {numeric('Traffic quota', 'quotaGb', 'GB')}
        <Field label="Expires" hint="Access ends after this day (UTC).">
          {(id) => <Input id={id} type="date" value={form.expires} onChange={set('expires')} />}
        </Field>
        {numeric('Devices', 'maxIps', 'IPs', 'Distinct client IPs at once.')}
        {numeric('Connections', 'maxConns', 'conns', 'Open connections at once.')}
        {numeric('Download speed', 'downMbit', 'Mbit/s')}
        {numeric('Upload speed', 'upMbit', 'Mbit/s')}
        <button type="submit" hidden />
        {error && (
          <div className="col-span-2">
            <ErrorBox>{error}</ErrorBox>
          </div>
        )}
      </form>
    </Dialog>
  )
}

// --- Share -------------------------------------------------------------------

function ShareDialog({ user, onClose }: { user: TelemtUser; onClose: () => void }) {
  const links = user.links.tls.length ? user.links.tls : [...user.links.secure, ...user.links.classic]
  const [index, setIndex] = useState(0)
  const link = links[index]
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Share ${user.username}'s proxy`}
      description="Anyone with this link can use the proxy as this user."
    >
      {!link ? (
        <ErrorBox>The proxy has no link for this user yet. Try again in a few seconds.</ErrorBox>
      ) : (
        <div className="flex flex-col items-center gap-4">
          <QrCode text={toWebLink(link)} size={220} />
          {links.length > 1 && (
            <div className="flex flex-wrap justify-center gap-1">
              {links.map((l, i) => (
                <Button
                  key={l}
                  size="sm"
                  variant={i === index ? 'primary' : 'secondary'}
                  onClick={() => setIndex(i)}
                >
                  {new URL(toWebLink(l)).searchParams.get('server')}
                </Button>
              ))}
            </div>
          )}
          <div className="w-full break-all rounded-md bg-card2 p-3 font-mono text-xs">{link}</div>
          <div className="flex flex-wrap justify-center gap-2">
            <CopyButton text={link} label="Copy tg:// link" />
            <CopyButton text={toWebLink(link)} label="Copy t.me link" />
          </div>
        </div>
      )}
    </Dialog>
  )
}

// --- Confirm -----------------------------------------------------------------

function Confirm({
  title,
  body,
  action,
  danger,
  onConfirm,
  onClose,
}: {
  title: string
  body: string
  action: string
  danger?: boolean
  onConfirm: () => Promise<void>
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  return (
    <Dialog
      open
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant={danger ? 'danger' : 'primary'}
            busy={busy}
            onClick={async () => {
              setBusy(true)
              try {
                await onConfirm()
                onClose()
              } catch (e) {
                setError(message(e))
                setBusy(false)
              }
            }}
          >
            {action}
          </Button>
        </>
      }
    >
      <p className="text-[13px] text-muted">{body}</p>
      {error && (
        <div className="mt-3">
          <ErrorBox>{error}</ErrorBox>
        </div>
      )}
    </Dialog>
  )
}

// --- Tab -----------------------------------------------------------------------

type Modal =
  | { kind: 'add' }
  | { kind: 'edit' | 'share' | 'rotate' | 'reset' | 'delete'; user: TelemtUser }

export function UsersTab({ server }: { server: Server }) {
  const toast = useToast()
  const { data, error, loading, reload } = usePoll(() => usersApi.list(server.id), 10000, [
    server.id,
  ])
  const [query, setQuery] = useState('')
  const [modal, setModal] = useState<Modal | null>(null)
  const [toggling, setToggling] = useState<string | null>(null)

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    return (data ?? []).filter((u) => !q || u.username.toLowerCase().includes(q))
  }, [data, query])

  const totals = useMemo(() => {
    const list = data ?? []
    return {
      users: list.length,
      online: list.filter((u) => u.current_connections > 0).length,
      connections: list.reduce((n, u) => n + u.current_connections, 0),
      traffic: list.reduce((n, u) => n + u.total_octets, 0),
    }
  }, [data])

  const toggle = async (user: TelemtUser, enabled: boolean) => {
    setToggling(user.username)
    try {
      await usersApi.setEnabled(server.id, user.username, enabled)
      toast(`${user.username} ${enabled ? 'enabled' : 'disabled'}`)
      await reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setToggling(null)
    }
  }

  const actions = (user: TelemtUser) => [
    { label: 'Share link', icon: <QrCodeIcon />, onSelect: () => setModal({ kind: 'share', user }) },
    {
      label: 'Edit limits',
      icon: <SlidersHorizontal />,
      onSelect: () => setModal({ kind: 'edit', user }),
    },
    { label: 'New secret', icon: <KeyRound />, onSelect: () => setModal({ kind: 'rotate', user }) },
    {
      label: 'Reset quota',
      icon: <RotateCcw />,
      onSelect: () => setModal({ kind: 'reset', user }),
    },
    {
      label: 'Delete user',
      icon: <Trash2 />,
      danger: true,
      onSelect: () => setModal({ kind: 'delete', user }),
    },
  ]

  const stat = (label: string, value: string | number) => (
    <Card className="px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div className="tabular mt-0.5 text-lg font-semibold">{value}</div>
    </Card>
  )

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {stat('Users', totals.users)}
        {stat('Online now', totals.online)}
        {stat('Connections', totals.connections)}
        {stat('Traffic', formatBytes(totals.traffic))}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-[200px] flex-1">
          <Search className="pointer-events-none absolute left-3 top-2.5 h-4 w-4 text-faint" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search users"
            className="pl-9"
            aria-label="Search users"
          />
        </div>
        <Button variant="primary" onClick={() => setModal({ kind: 'add' })}>
          <Plus className="h-4 w-4" /> Add user
        </Button>
      </div>

      {error && <ErrorBox>{error}</ErrorBox>}
      {loading && !data && <Spinner />}

      {data && shown.length === 0 && (
        <Card className="flex flex-col items-center gap-2 px-6 py-10 text-center text-[13px] text-muted">
          {data.length === 0 ? <Users className="h-6 w-6" /> : <SearchX className="h-6 w-6" />}
          {data.length === 0 ? 'No users yet. Add one to get a proxy link.' : 'No user matches.'}
        </Card>
      )}

      {shown.length > 0 && (
        <Card className="overflow-hidden">
          {/* Table on wide screens */}
          <table className="hidden w-full text-left text-[13px] md:table">
            <thead className="border-b border-border text-xs text-muted">
              <tr>
                <th className="px-4 py-2.5 font-medium">User</th>
                <th className="px-4 py-2.5 font-medium">Status</th>
                <th className="px-4 py-2.5 font-medium">Connections</th>
                <th className="px-4 py-2.5 font-medium">Devices</th>
                <th className="px-4 py-2.5 font-medium">Traffic</th>
                <th className="px-4 py-2.5 font-medium">Expires</th>
                <th className="px-4 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {shown.map((u) => (
                <tr key={u.username} className="border-b border-border last:border-0">
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-3">
                      <Switch
                        checked={u.enabled}
                        disabled={toggling === u.username}
                        onChange={(on) => void toggle(u, on)}
                        label={`${u.enabled ? 'Disable' : 'Enable'} ${u.username}`}
                      />
                      <span className="font-medium">{u.username}</span>
                    </div>
                  </td>
                  <td className="px-4 py-3">
                    <UserState user={u} />
                  </td>
                  <td className="tabular px-4 py-3">
                    {u.current_connections}
                    {u.max_tcp_conns !== null && (
                      <span className="text-faint"> / {u.max_tcp_conns}</span>
                    )}
                  </td>
                  <td className="tabular px-4 py-3" title={u.active_unique_ips_list.join('\n')}>
                    {u.active_unique_ips}
                    {u.max_unique_ips !== null && (
                      <span className="text-faint"> / {u.max_unique_ips}</span>
                    )}
                  </td>
                  <td className="px-4 py-3">
                    <Traffic user={u} />
                  </td>
                  <td className="px-4 py-3 text-muted">
                    {u.expiration_rfc3339 ? (
                      <span
                        className={cx(expired(u) && 'text-status-danger')}
                        title={u.expiration_rfc3339}
                      >
                        {relativeTime(u.expiration_rfc3339)}
                      </span>
                    ) : (
                      'never'
                    )}
                  </td>
                  <td className="px-4 py-3">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => setModal({ kind: 'share', user: u })}
                      >
                        <QrCodeIcon className="h-4 w-4" /> Share
                      </Button>
                      <Menu
                        trigger={
                          <Button size="icon" variant="ghost" aria-label={`Actions for ${u.username}`}>
                            <Ellipsis className="h-4 w-4" />
                          </Button>
                        }
                        items={actions(u)}
                      />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {/* Cards on phones */}
          <ul className="divide-y divide-border md:hidden">
            {shown.map((u) => (
              <li key={u.username} className="flex flex-col gap-3 p-4">
                <div className="flex items-center gap-3">
                  <Switch
                    checked={u.enabled}
                    disabled={toggling === u.username}
                    onChange={(on) => void toggle(u, on)}
                    label={`${u.enabled ? 'Disable' : 'Enable'} ${u.username}`}
                  />
                  <span className="flex-1 truncate font-medium">{u.username}</span>
                  <UserState user={u} />
                  <Menu
                    trigger={
                      <Button size="icon" variant="ghost" aria-label={`Actions for ${u.username}`}>
                        <Ellipsis className="h-4 w-4" />
                      </Button>
                    }
                    items={actions(u)}
                  />
                </div>
                <div className="flex flex-wrap items-end gap-x-5 gap-y-2 text-xs text-muted">
                  <span className="inline-flex items-center gap-1">
                    <Activity className="h-3.5 w-3.5" /> {u.current_connections} conns
                  </span>
                  <span>{u.active_unique_ips} devices</span>
                  <span>
                    {u.expiration_rfc3339
                      ? `expires ${relativeTime(u.expiration_rfc3339)}`
                      : 'no expiry'}
                  </span>
                  <Traffic user={u} />
                </div>
              </li>
            ))}
          </ul>
        </Card>
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
          title={`New secret for ${modal.user.username}?`}
          body="Their current link stops working. You'll share the new link with them."
          action="Make new secret"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            const res = await usersApi.rotateSecret(server.id, modal.user.username)
            toast(`${modal.user.username} has a new secret`)
            void reload()
            setTimeout(() => setModal({ kind: 'share', user: res.user }))
          }}
        />
      )}
      {modal?.kind === 'reset' && (
        <Confirm
          title={`Reset ${modal.user.username}'s quota?`}
          body="Their quota counter starts again from zero, so a used-up quota is available again. Total traffic is not affected."
          action="Reset quota"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.resetQuota(server.id, modal.user.username)
            toast(`${modal.user.username}'s quota was reset`)
            void reload()
          }}
        />
      )}
      {modal?.kind === 'delete' && (
        <Confirm
          danger
          title={`Delete ${modal.user.username}?`}
          body="Their link stops working and the user is removed from the server."
          action="Delete user"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.remove(server.id, modal.user.username)
            toast(`${modal.user.username} deleted`)
            void reload()
          }}
        />
      )}
    </div>
  )
}
