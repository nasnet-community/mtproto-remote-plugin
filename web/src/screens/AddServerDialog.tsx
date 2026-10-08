import { useState, type FormEvent } from 'react'
import { CheckResult } from '../components/cover'
import { DOMESTIC_HINT, JobSteps } from '../components/job'
import {
  HostKeyCheck,
  LoginFields,
  emptyLogin,
  keyNeedsTrust,
  keyTrusted,
  loginError,
  toLogin,
  withoutSecrets,
  type LoginForm,
} from '../components/login'
import { ApiError, api, type AppConfig, type CoverCheck, type InstallRequest, type ProbeResult } from '../lib/api'
import { DOMAIN, streamsLeft, validUserName } from '../lib/format'
import { useJob } from '../hooks/useJob'
import { Button, Dialog, ErrorBox, Field, Input, useToast } from '../components/ui'

type Stage = 'form' | 'hostkey' | 'installing'

const EMPTY = { host: '', name: '', coverDomain: '', firstUser: 'user1' }

interface Props {
  config: AppConfig | null
  onClose: () => void
  /** The install job ended with the server saved (its link may still be down). */
  onInstalled: () => void
  onOpenServer: (serverId: number) => void
}

/** The username is not a secret, so the next add starts from it. */
let lastUser = 'root'

// Mounted only while open: closing it however you like drops everything typed, the login included.
export function AddServerDialog({ open, ...props }: Props & { open: boolean }) {
  return open ? <AddServer {...props} /> : null
}

function AddServer({ config, onClose, onInstalled, onOpenServer }: Props) {
  const toast = useToast()
  const [form, setForm] = useState(EMPTY)
  const [login, setLogin] = useState<LoginForm>(() => emptyLogin(22, lastUser))
  const [stage, setStage] = useState<Stage>('form')
  const [key, setKey] = useState<ProbeResult | null>(null)
  const [ackChanged, setAckChanged] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // the request tests the cover site from the server before anything is installed
  const [testing, setTesting] = useState(false)
  const [cover, setCover] = useState<{ domain: string; check: CoverCheck } | null>(null)
  const [jobId, setJobId] = useState<string | null>(null)
  const name = form.name.trim() || form.host.trim()
  const coverDomain = form.coverDomain.trim().toLowerCase()
  const coverFailed = cover?.domain === coverDomain ? cover.check : null
  const left = config ? streamsLeft(config) : null
  // null follows the default, which may only arrive after the dialog opened
  const [typedStreams, setTypedStreams] = useState<string | null>(null)
  const streamsText = typedStreams ?? String(Math.max(0, Math.min(config?.defaultMaxStreams ?? 2000, left ?? Infinity)))

  const job = useJob(jobId, (j) => {
    if (j.state === 'ok') toast(`${name} added`)
    else toast('Installation failed', 'error')
    if (j.serverId !== undefined) onInstalled()
  })

  const set = (k: keyof typeof EMPTY) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: e.target.value }))

  const request = (fingerprint: string): InstallRequest => ({
    ...toLogin(login, fingerprint),
    host: form.host.trim(),
    name,
    coverDomain,
    maxStreams: Number(streamsText),
    firstUser: form.firstUser.trim() || undefined,
  })

  const install = async (fingerprint: string) => {
    setBusy(true)
    setTesting(true)
    setError(null)
    setCover(null)
    try {
      const { jobId } = await api.install(request(fingerprint))
      setLogin(withoutSecrets)
      setJobId(jobId)
      setStage('installing')
    } catch (e) {
      // a failed cover test keeps the form as it was, login included: only the site needs changing
      const check = e instanceof ApiError ? (e.body.cover as CoverCheck | undefined) : undefined
      if (check) setCover({ domain: coverDomain, check })
      else setError(e instanceof Error ? e.message : String(e))
      setStage('form')
    } finally {
      setBusy(false)
      setTesting(false)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    const first = form.firstUser.trim()
    const streams = Number(streamsText)
    const problem =
      loginError(login) ??
      (!DOMAIN.test(form.coverDomain.trim())
        ? 'Give the cover site as a domain name, like www.example.com.'
        : first && !validUserName(first)
          ? "Name the first user with letters, digits, '.', '_' or '-'."
          : !Number.isInteger(streams) || streams < 100
            ? 'Give a stream limit of at least 100.'
            : left !== null && streams > left
              ? `Only ${Math.max(left, 0)} streams are left on this router; lower another server's limit first.`
              : null)
    if (problem) return setError(problem)
    lastUser = login.sshUser.trim()
    setBusy(true)
    setError(null)
    try {
      const probe = await api.probe({
        host: form.host.trim(),
        sshPort: Number(login.sshPort),
        sshUser: login.sshUser.trim(),
      })
      setKey(probe)
      setAckChanged(false)
      if (!keyNeedsTrust(probe)) {
        setBusy(false)
        await install(probe.fingerprint)
      } else {
        setStage('hostkey')
        setBusy(false)
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
    }
  }

  const finished = job && job.state !== 'running'
  const running = stage === 'installing' && !finished

  const footer =
    stage === 'form' ? (
      <>
        <Button onClick={onClose} disabled={testing}>
          Cancel
        </Button>
        <Button variant="primary" type="submit" form="add-server" busy={busy}>
          {testing ? 'Testing the cover site…' : 'Connect and install'}
        </Button>
      </>
    ) : stage === 'hostkey' ? (
      <>
        <Button onClick={() => setStage('form')} disabled={testing}>
          Back
        </Button>
        <Button
          variant={key?.status === 'changed' ? 'danger' : 'primary'}
          disabled={!key || !keyTrusted(key, ackChanged)}
          busy={busy}
          onClick={() => key && install(key.fingerprint)}
        >
          {testing ? 'Testing the cover site…' : 'Trust and install'}
        </Button>
      </>
    ) : finished && job.serverId !== undefined ? (
      <>
        {job.state === 'failed' && <Button onClick={onClose}>Close</Button>}
        <Button variant="primary" onClick={() => onOpenServer(job.serverId!)}>
          Open server
        </Button>
      </>
    ) : finished ? (
      <>
        <Button onClick={onClose}>Close</Button>
        <Button
          variant="primary"
          onClick={() => {
            setJobId(null)
            setStage('form')
          }}
        >
          Try again
        </Button>
      </>
    ) : null

  return (
    <Dialog
      open
      // once the test passes the install starts, so it can't be walked away from mid-test
      onClose={running || testing ? () => {} : onClose}
      title={
        stage === 'installing'
          ? job?.state === 'ok'
            ? 'Server added'
            : job?.state === 'failed'
              ? 'Installation failed'
              : 'Installing'
          : 'Add a server'
      }
      description={
        stage === 'form' ? 'The login is used once for the install and never stored.' : undefined
      }
      footer={footer}
    >
      {stage === 'form' && (
        <form id="add-server" onSubmit={submit} className="grid grid-cols-6 gap-3" autoComplete="off">
          <LoginFields
            form={login}
            onChange={setLogin}
            host={
              <Field label="Server address" hint={DOMESTIC_HINT} className="col-span-4">
                {(id) => (
                  <Input id={id} required value={form.host} onChange={set('host')} placeholder="203.0.113.10" spellCheck={false} />
                )}
              </Field>
            }
          />
          <Field
            label="Cover site"
            hint="A real HTTPS site with TLS 1.3 and HTTP/2, tested from the server before anything is installed."
            error={coverFailed && `${cover!.domain} doesn't work as a cover site from this server. Try another.`}
            className="col-span-4"
          >
            {(id) => (
              <Input id={id} required value={form.coverDomain} onChange={set('coverDomain')} spellCheck={false} autoCapitalize="off" />
            )}
          </Field>
          <Field
            label="Stream limit"
            hint={left !== null ? `${Math.max(left, 0)} left on this router.` : undefined}
            className="col-span-2"
          >
            {(id) => (
              <Input id={id} type="number" value={streamsText} onChange={(e) => setTypedStreams(e.target.value)} />
            )}
          </Field>
          {coverFailed && (
            <div className="col-span-6">
              <CheckResult check={coverFailed} />
            </div>
          )}
          <Field label="Name" hint="Optional; shown in this list." className="col-span-3">
            {(id) => <Input id={id} value={form.name} onChange={set('name')} placeholder={form.host || 'Frankfurt VPS'} maxLength={100} />}
          </Field>
          <Field label="First user" hint="Optional; add more later." className="col-span-3">
            {(id) => <Input id={id} value={form.firstUser} onChange={set('firstUser')} spellCheck={false} maxLength={32} />}
          </Field>
          {error && (
            <div className="col-span-6">
              <ErrorBox>{error}</ErrorBox>
            </div>
          )}
        </form>
      )}

      {stage === 'hostkey' && key && (
        <div className="flex flex-col gap-3">
          <HostKeyCheck host={form.host} probe={key} acked={ackChanged} onAck={setAckChanged} />
          {error && <ErrorBox>{error}</ErrorBox>}
        </div>
      )}

      {stage === 'installing' && (
        <div className="flex flex-col gap-4">
          <JobSteps job={job} />
          {job?.state === 'ok' && (
            <p className="text-[13px] text-muted">The agent is running and its link is up.</p>
          )}
          {job?.state === 'failed' && job.serverId !== undefined && (
            <p className="text-[13px] text-muted">
              The server is saved. Fix the cause, then run a repair from its page.
            </p>
          )}
        </div>
      )}
    </Dialog>
  )
}
