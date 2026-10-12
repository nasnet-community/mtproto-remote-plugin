import { useId, type KeyboardEvent, type ReactNode } from 'react'
import type { Login, ProbeResult } from '../lib/api'
import { KeyRound, TriangleAlert } from './icons'
import { Field, Input, Textarea, cx } from './ui'

// Lives only in component state for one submission; never persisted.
export interface LoginForm {
  sshPort: string
  sshUser: string
  method: 'password' | 'key'
  password: string
  privateKey: string
  passphrase: string
}

// a half-saved server has port 0 and no user
export const emptyLogin = (sshPort = 22, sshUser = 'root'): LoginForm => ({
  sshPort: String(sshPort || 22),
  sshUser: sshUser || 'root',
  method: 'password',
  password: '',
  privateKey: '',
  passphrase: '',
})

export const withoutSecrets = (f: LoginForm): LoginForm => ({
  ...f,
  password: '',
  privateKey: '',
  passphrase: '',
})

export function loginError(f: LoginForm): string | null {
  const port = Number(f.sshPort)
  if (!Number.isInteger(port) || port < 1 || port > 65535) return 'The SSH port is a number from 1 to 65535.'
  if (!f.sshUser.trim()) return 'Give the SSH username.'
  if (f.method === 'password' && !f.password) return 'Give the password.'
  if (f.method === 'key' && !f.privateKey.trim()) return 'Paste the private key.'
  return null
}

/** The login to send: only the secret that was chosen. */
export function toLogin(f: LoginForm, hostKey: string): Login {
  const login: Login = { sshPort: Number(f.sshPort), sshUser: f.sshUser.trim(), hostKey }
  if (f.method === 'password') login.password = f.password
  else {
    login.privateKey = f.privateKey
    if (f.passphrase) login.passphrase = f.passphrase
  }
  return login
}

const METHODS = [
  ['password', 'Password'],
  ['key', 'Private key'],
] as const

/** SSH port, user and a password or private key, laid out on a 6-column grid. */
export function LoginFields({
  form,
  onChange,
  host,
  afterHost,
}: {
  form: LoginForm
  onChange: (f: LoginForm) => void
  /** A host field to put in front of the port, on the same row. */
  host?: ReactNode
  /** A line under the host and port row. */
  afterHost?: ReactNode
}) {
  const methodId = useId()
  const set = (k: keyof LoginForm) => (e: { target: { value: string } }) => onChange({ ...form, [k]: e.target.value })
  // arrow keys move between the two choices, as in any radio group
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(e.key)) return
    e.preventDefault()
    const next = form.method === 'password' ? 1 : 0
    onChange({ ...form, method: METHODS[next][0] })
    e.currentTarget.querySelectorAll<HTMLElement>('[role="radio"]')[next]?.focus()
  }
  return (
    <>
      {host}
      <Field label="SSH port" className="col-span-2">
        {(p) => <Input {...p} required type="number" min={1} max={65535} value={form.sshPort} onChange={set('sshPort')} />}
      </Field>
      {afterHost}
      <Field label="Username" className={host ? 'col-span-3' : 'col-span-4'}>
        {(p) => <Input {...p} required value={form.sshUser} onChange={set('sshUser')} spellCheck={false} autoCapitalize="off" />}
      </Field>
      <div className={cx('flex flex-col gap-1.5', host ? 'col-span-3' : 'col-span-6')}>
        <span id={methodId} className="text-[13px] font-medium text-muted">
          Log in with
        </span>
        <div
          role="radiogroup"
          aria-labelledby={methodId}
          onKeyDown={onKey}
          className="flex h-11 rounded-md border border-border-strong p-0.5 sm:h-9"
        >
          {METHODS.map(([m, text]) => (
            <button
              key={m}
              type="button"
              role="radio"
              aria-checked={form.method === m}
              tabIndex={form.method === m ? 0 : -1}
              onClick={() => onChange({ ...form, method: m })}
              className={cx(
                'flex-1 whitespace-nowrap rounded-[6px] px-1 text-[12.5px] font-medium transition sm:px-2 sm:text-[13px]',
                'focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent',
                form.method === m ? 'bg-card2 text-foreground shadow-sm' : 'text-muted hover:text-foreground',
              )}
            >
              {text}
            </button>
          ))}
        </div>
      </div>
      {form.method === 'password' ? (
        <Field label="Password" className="col-span-6">
          {(p) => <Input {...p} required type="password" value={form.password} onChange={set('password')} autoComplete="new-password" />}
        </Field>
      ) : (
        <>
          <Field label="Private key" hint="OpenSSH or PEM text." className="col-span-6">
            {(p) => (
              <Textarea
                {...p}
                required
                value={form.privateKey}
                onChange={set('privateKey')}
                spellCheck={false}
                autoComplete="off"
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              />
            )}
          </Field>
          <Field label="Key passphrase" hint="Only if the key has one." className="col-span-6">
            {(p) => <Input {...p} type="password" value={form.passphrase} onChange={set('passphrase')} autoComplete="new-password" />}
          </Field>
        </>
      )}
    </>
  )
}

/** A first key is trusted and pinned, like ssh does; only a key that changed stops to ask. */
export const keyNeedsTrust = (p: ProbeResult) => p.status === 'changed'

/** A changed key needs the box ticked. */
export const keyTrusted = (p: ProbeResult, acked: boolean) => p.status !== 'changed' || acked

/** The warning for a host key that differs from the pinned one. */
export function HostKeyCheck({
  host,
  probe,
  acked,
  onAck,
}: {
  host: string
  probe: ProbeResult
  acked: boolean
  onAck: (acked: boolean) => void
}) {
  return (
    <div className="flex flex-col gap-3">
      <div className="flex gap-3 rounded-md border border-warn bg-warn-tint p-3">
        <TriangleAlert className="h-5 w-5 shrink-0 text-warn-text" />
        <p className="text-[13px]">
          <b>{host}</b> shows a different key than last time. That happens after a reinstall — or when someone
          is intercepting the connection.
        </p>
      </div>
      <div className="flex items-center gap-2 break-all rounded-md bg-card2 p-3 font-mono text-xs">
        <KeyRound className="h-4 w-4 shrink-0 text-muted" />
        {probe.fingerprint}
      </div>
      <label className="flex min-h-[44px] items-center gap-2 text-[13px] sm:min-h-0">
        <input type="checkbox" className="h-4 w-4" checked={acked} onChange={(e) => onAck(e.target.checked)} />
        I know why the key changed
      </label>
    </div>
  )
}
