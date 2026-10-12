import { useState } from 'react'
import { users as usersApi, type Server, type User } from '../lib/api'
import { GB, dayText, daysFromToday, limitText, parseLimit, validSecret, validUserName } from '../lib/format'
import { Button, Chip, CopyButton, Dialog, Disclosure, ErrorBox, Field, Input, QrCode, useToast } from '../components/ui'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

type Preset = 'never' | '7' | '30' | '90' | 'date'

const PRESETS: [Preset, string][] = [
  ['never', 'Never'],
  ['7', '7 days'],
  ['30', '30 days'],
  ['90', '90 days'],
  ['date', 'Pick a date'],
]

interface Form {
  name: string
  note: string
  quotaGb: string
  maxIps: string
  preset: Preset
  date: string
  secret: string
}

const toForm = (u?: User): Form => ({
  name: u?.name ?? '',
  note: u?.note ?? '',
  quotaGb: limitText(u?.quotaBytes, GB),
  maxIps: limitText(u?.maxIps),
  preset: u?.expiresAt ? 'date' : u ? 'never' : '30',
  date: u?.expiresAt?.slice(0, 10) ?? '',
  secret: '',
})

/** Add a user, or edit one: limits, note and expiry. */
export function UserDialog({
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
  const [bad, setBad] = useState<Partial<Record<keyof Form, string>>>({})
  const set = (k: keyof Form) => (e: { target: { value: string } }) => setForm((f) => ({ ...f, [k]: e.target.value }))

  // an untouched expiry is sent back as it was, to the second
  const kept = user?.expiresAt && form.preset === 'date' && form.date === user.expiresAt.slice(0, 10) ? user.expiresAt : null
  const expiresAt =
    form.preset === 'never' ? null
    : form.preset === 'date' ? kept ?? (form.date ? `${form.date}T23:59:59Z` : '')
    : daysFromToday(Number(form.preset))

  const save = async () => {
    setError(null)
    const quotaBytes = parseLimit(form.quotaGb, GB)
    const maxIps = parseLimit(form.maxIps)
    const name = form.name.trim()
    const secret = form.secret.trim()
    const errs: typeof bad = {}
    if (!user && !validUserName(name)) errs.name = "Use 1–32 letters, digits, '.', '_' or '-' for the name."
    if (quotaBytes === 'invalid') errs.quotaGb = 'Traffic quota must be a positive number, or empty for no limit.'
    if (maxIps === 'invalid') errs.maxIps = 'Devices must be a positive number, or empty for no limit.'
    if (expiresAt === '') errs.date = 'Pick the last day of access.'
    if (secret && !validSecret(secret)) errs.secret = 'Paste the 32-character secret, or one that starts with dd or ee.'
    setBad(errs)
    if (Object.keys(errs).length || quotaBytes === 'invalid' || maxIps === 'invalid') return
    setBusy(true)
    try {
      const note = form.note.trim()
      if (user) {
        await usersApi.update(server.id, user.name, { quotaBytes, maxIps, expiresAt, note })
        toast(`${user.name} updated`)
        onSaved()
      } else {
        // a new user has nothing to clear, so send only what is set
        const created = await usersApi.create(server.id, {
          name,
          ...(note ? { note } : {}),
          ...(quotaBytes ? { quotaBytes } : {}),
          ...(maxIps ? { maxIps } : {}),
          ...(expiresAt ? { expiresAt } : {}),
          ...(secret ? { secret } : {}),
        })
        toast(`${name} added`)
        onSaved(created)
      }
    } catch (e) {
      setError(message(e))
      setBusy(false)
    }
  }

  const unit = (u: string) => <span className="pointer-events-none absolute right-3 top-3 text-xs text-faint sm:top-2">{u}</span>

  return (
    <Dialog
      onClose={onClose}
      title={user ? `Edit ${user.name}` : 'Add a user'}
      description={user ? 'Leave a limit empty for no limit.' : 'Leave a limit empty for no limit. A secret is made for them.'}
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
          <Field label="Name" hint="Letters, digits, '.', '_' or '-'." error={bad.name} className="col-span-2">
            {(p) => <Input {...p} value={form.name} onChange={set('name')} maxLength={32} spellCheck={false} autoCapitalize="off" placeholder="alice" />}
          </Field>
        )}
        <Field
          label={
            <>
              Note <span className="font-normal text-faint">(optional)</span>
            </>
          }
          className="col-span-2"
        >
          {(p) => <Input {...p} value={form.note} onChange={set('note')} maxLength={80} placeholder="Mom's tablet" />}
        </Field>
        <Field label="Traffic quota" error={bad.quotaGb}>
          {(p) => (
            <div className="relative">
              <Input {...p} inputMode="decimal" value={form.quotaGb} onChange={set('quotaGb')} placeholder="No limit" className="pr-10" />
              {unit('GB')}
            </div>
          )}
        </Field>
        <Field label="Devices" error={bad.maxIps}>
          {(p) => (
            <div className="relative">
              <Input {...p} inputMode="numeric" value={form.maxIps} onChange={set('maxIps')} placeholder="No limit" className="pr-16" />
              {unit('at once')}
            </div>
          )}
        </Field>
        <fieldset className="col-span-2 flex flex-col gap-2">
          <legend className="mb-1.5 text-[13px] font-medium text-muted">Expires</legend>
          <div className="flex flex-wrap gap-2">
            {PRESETS.map(([p, text]) => (
              <Chip key={p} on={form.preset === p} onClick={() => setForm((f) => ({ ...f, preset: p }))}>
                {text}
              </Chip>
            ))}
          </div>
          {form.preset === 'date' && (
            <Field label="Last day" error={bad.date}>
              {(p) => <Input {...p} type="date" value={form.date} onChange={set('date')} />}
            </Field>
          )}
          <p className="text-xs text-faint">
            {expiresAt ? `Access ends ${dayText(expiresAt)} at ${expiresAt.slice(11, 16)} UTC.` : expiresAt === null ? 'No expiry.' : 'Pick the last day of access.'}
          </p>
        </fieldset>
        {!user && (
          <Disclosure title="Advanced" className="col-span-2 border-t border-border pt-3">
            <Field
              label="Use an existing secret"
              hint="For someone moving from another proxy at the same address and cover site: their old link keeps working."
              error={bad.secret}
            >
              {(p) => (
                <Input
                  {...p}
                  value={form.secret}
                  onChange={set('secret')}
                  spellCheck={false}
                  autoCapitalize="off"
                  autoComplete="off"
                  className="font-mono text-xs"
                  placeholder="32 hex characters, or a dd or ee secret"
                />
              )}
            </Field>
          </Disclosure>
        )}
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

/** The QR code and the link it holds, ready to send. */
export function ShareDialog({ user, onClose }: { user: User; onClose: () => void }) {
  return (
    <Dialog onClose={onClose} title={`Share ${user.name}'s proxy`} description={`Anyone with this link can use the proxy as ${user.name}.`}>
      <div className="flex flex-col items-center gap-4">
        <QrCode text={user.links.https} size={220} />
        <div className="w-full break-all rounded-md border border-border bg-card2 p-3 font-mono text-xs" data-link>
          {user.links.https}
        </div>
        <div className="grid w-full gap-2">
          <CopyButton text={user.links.https} label="Copy link" variant="primary" size="md" className="w-full" />
          <CopyButton text={user.links.tg} label="Copy tg:// link" size="md" className="w-full" />
        </div>
        {user.links.extra.length > 0 && (
          <div className="w-full border-t border-border pt-3">
            <div className="text-[13px] font-medium">Other cover names</div>
            <p className="text-xs text-faint">If the link above gets blocked, these still work.</p>
            <ul className="mt-1 divide-y divide-border">
              {user.links.extra.map((x) => (
                <li key={x.domain} className="flex items-center justify-between gap-2 py-1.5">
                  <span className="min-w-0 truncate font-mono text-xs">{x.domain}</span>
                  <CopyButton text={x.https} label="Copy" name={`Copy the ${x.domain} link`} />
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Dialog>
  )
}
