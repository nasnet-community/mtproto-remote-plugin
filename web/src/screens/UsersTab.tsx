import { useMemo, useState } from 'react'
import {
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
import { users as usersApi, type CreateUser, type Server, type User } from '../lib/api'
import { GB, formatBytes, limitText, parseLimit, relativeTime, validUserName } from '../lib/format'
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
  Notice,
  QrCode,
  Spinner,
  Switch,
  cx,
  useToast,
} from '../components/ui'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

// The agent reports bytes per user, not live sessions, so usage is all we show.
function Traffic({ user }: { user: User }) {
  return (
    <div className="flex flex-col">
      <span className="tabular">{formatBytes(user.up + user.down)}</span>
      <span className="tabular text-xs text-faint">
        ↑ {formatBytes(user.up)} ↓ {formatBytes(user.down)}
        {user.quotaBytes > 0 && ` · quota ${formatBytes(user.quotaBytes)}`}
      </span>
    </div>
  )
}

const STATE = {
  active: ['online', 'active'],
  disabled: ['idle', 'disabled'],
  expired: ['danger', 'expired'],
  'over-quota': ['warn', 'over quota'],
} as const

function UserState({ user }: { user: User }) {
  const [tone, label] = STATE[user.state] ?? ['idle', user.state]
  return <Badge tone={tone}>{label}</Badge>
}

function Expiry({ user }: { user: User }) {
  if (!user.expiresAt) return <>never</>
  const past = new Date(user.expiresAt).getTime() < Date.now()
  return (
    <span className={cx(past && 'text-status-danger')} title={user.expiresAt}>
      {relativeTime(user.expiresAt)}
    </span>
  )
}

// --- Add / edit ------------------------------------------------------------

interface LimitForm {
  name: string
  maxIps: string
  quotaGb: string
  expires: string
}

const toForm = (user?: User): LimitForm => ({
  name: user?.name ?? '',
  maxIps: limitText(user?.maxIps),
  quotaGb: limitText(user?.quotaBytes, GB),
  expires: user?.expiresAt ? user.expiresAt.slice(0, 10) : '',
})

function toLimits(f: LimitForm): Required<Omit<CreateUser, 'name' | 'enabled'>> | string {
  const quotaBytes = parseLimit(f.quotaGb, GB)
  if (quotaBytes === 'invalid') return 'Traffic quota must be a positive number, or empty for no limit.'
  const maxIps = parseLimit(f.maxIps)
  if (maxIps === 'invalid') return 'Devices must be a positive number, or empty for no limit.'
  // The date picker gives a day; the user stays valid through its end (UTC).
  return { quotaBytes, maxIps, expiresAt: f.expires ? `${f.expires}T23:59:59Z` : null }
}

function UserDialog({
  server,
  user,
  onClose,
  onSaved,
}: {
  server: Server
  user?: User
  onClose: () => void
  onSaved: (created?: User) => void
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
    const name = form.name.trim()
    if (!user && !validUserName(name)) {
      return setError("Use 1–32 letters, digits, '.', '_' or '-' for the name.")
    }
    setBusy(true)
    try {
      if (user) {
        await usersApi.update(server.id, user.name, limits)
        toast(`${user.name} updated`)
        onSaved()
      } else {
        // a new user has nothing to clear, so send only the limits that are set
        const created = await usersApi.create(server.id, {
          name,
          ...(limits.quotaBytes ? { quotaBytes: limits.quotaBytes } : {}),
          ...(limits.maxIps ? { maxIps: limits.maxIps } : {}),
          ...(limits.expiresAt ? { expiresAt: limits.expiresAt } : {}),
        })
        toast(`${name} added`)
        onSaved(created)
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
            className="pr-12"
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
      title={user ? `Limits for ${user.name}` : 'Add a user'}
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
                value={form.name}
                onChange={set('name')}
                maxLength={32}
                spellCheck={false}
                autoCapitalize="off"
                placeholder="alice"
              />
            )}
          </Field>
        )}
        {numeric('Traffic quota', 'quotaGb', 'GB', 'Upload and download together.')}
        <Field label="Expires" hint="Access ends after this day (UTC).">
          {(id) => <Input id={id} type="date" value={form.expires} onChange={set('expires')} />}
        </Field>
        {numeric('Devices', 'maxIps', 'IPs', 'Distinct client addresses at once.')}
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

function ShareDialog({ user, onClose }: { user: User; onClose: () => void }) {
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Share ${user.name}'s proxy`}
      description="Anyone with this link can use the proxy as this user."
    >
      <div className="flex flex-col items-center gap-4">
        <QrCode text={user.links.https} size={220} />
        <div className="w-full break-all rounded-md bg-card2 p-3 font-mono text-xs">{user.links.tg}</div>
        <div className="flex flex-wrap justify-center gap-2">
          <CopyButton text={user.links.tg} label="Copy tg:// link" />
          <CopyButton text={user.links.https} label="Copy t.me link" />
        </div>
      </div>
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
  | { kind: 'edit' | 'share' | 'rotate' | 'reset' | 'delete'; user: User }

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
    return (data ?? []).filter((u) => !q || u.name.toLowerCase().includes(q))
  }, [data, query])

  const totals = useMemo(() => {
    const list = data ?? []
    return {
      users: list.length,
      active: list.filter((u) => u.state === 'active').length,
      up: list.reduce((n, u) => n + u.up, 0),
      down: list.reduce((n, u) => n + u.down, 0),
    }
  }, [data])

  const toggle = async (user: User, enabled: boolean) => {
    setToggling(user.name)
    try {
      await usersApi.update(server.id, user.name, { enabled })
      toast(`${user.name} ${enabled ? 'enabled' : 'disabled'}`)
      await reload()
    } catch (e) {
      toast(message(e), 'error')
    } finally {
      setToggling(null)
    }
  }

  const actions = (user: User) => [
    { label: 'Share link', icon: <QrCodeIcon />, onSelect: () => setModal({ kind: 'share', user }) },
    {
      label: 'Edit limits',
      icon: <SlidersHorizontal />,
      onSelect: () => setModal({ kind: 'edit', user }),
    },
    { label: 'New secret', icon: <KeyRound />, onSelect: () => setModal({ kind: 'rotate', user }) },
    { label: 'Reset usage', icon: <RotateCcw />, onSelect: () => setModal({ kind: 'reset', user }) },
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

  const menu = (u: User) => (
    <Menu
      trigger={
        <Button size="icon" variant="ghost" aria-label={`Actions for ${u.name}`}>
          <Ellipsis className="h-4 w-4" />
        </Button>
      }
      items={actions(u)}
    />
  )

  const toggleSwitch = (u: User) => (
    <Switch
      checked={u.enabled}
      disabled={toggling === u.name}
      onChange={(on) => void toggle(u, on)}
      label={`${u.enabled ? 'Disable' : 'Enable'} ${u.name}`}
    />
  )

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {stat('Users', totals.users)}
        {stat('Active', totals.active)}
        {stat('Uploaded', formatBytes(totals.up))}
        {stat('Downloaded', formatBytes(totals.down))}
      </div>

      {server.link.state === 'down' && <Notice>Changes reach the server when its link is back.</Notice>}

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
                <th className="px-4 py-2.5 font-medium">Traffic</th>
                <th className="px-4 py-2.5 font-medium">Expires</th>
                <th className="px-4 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {shown.map((u) => (
                <tr key={u.name} className="border-b border-border last:border-0">
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-3">
                      {toggleSwitch(u)}
                      <span className="font-medium">{u.name}</span>
                    </div>
                  </td>
                  <td className="px-4 py-3">
                    <UserState user={u} />
                  </td>
                  <td className="px-4 py-3">
                    <Traffic user={u} />
                  </td>
                  <td className="px-4 py-3 text-muted">
                    <Expiry user={u} />
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
                      {menu(u)}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {/* Cards on phones */}
          <ul className="divide-y divide-border md:hidden">
            {shown.map((u) => (
              <li key={u.name} className="flex flex-col gap-3 p-4">
                <div className="flex items-center gap-3">
                  {toggleSwitch(u)}
                  <span className="flex-1 truncate font-medium">{u.name}</span>
                  <UserState user={u} />
                  {menu(u)}
                </div>
                <div className="flex flex-wrap items-end gap-x-5 gap-y-2 text-xs text-muted">
                  <Traffic user={u} />
                  <span>{u.expiresAt ? <>expires <Expiry user={u} /></> : 'no expiry'}</span>
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
          title={`New secret for ${modal.user.name}?`}
          body="Their current link stops working. You'll share the new link with them."
          action="Make new secret"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            const updated = await usersApi.rotateSecret(server.id, modal.user.name)
            toast(`${modal.user.name} has a new secret`)
            void reload()
            setTimeout(() => setModal({ kind: 'share', user: updated }))
          }}
        />
      )}
      {modal?.kind === 'reset' && (
        <Confirm
          title={`Reset ${modal.user.name}'s usage?`}
          body="Their upload and download counters start again from zero, which also lifts a used-up quota."
          action="Reset usage"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.resetUsage(server.id, modal.user.name)
            toast(`${modal.user.name}'s usage was reset`)
            void reload()
          }}
        />
      )}
      {modal?.kind === 'delete' && (
        <Confirm
          danger
          title={`Delete ${modal.user.name}?`}
          body="Their link stops working and the user is removed from the server."
          action="Delete user"
          onClose={() => setModal(null)}
          onConfirm={async () => {
            await usersApi.remove(server.id, modal.user.name)
            toast(`${modal.user.name} deleted`)
            void reload()
          }}
        />
      )}
    </div>
  )
}
