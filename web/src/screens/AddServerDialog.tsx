import { useState, type FormEvent } from 'react'
import { CheckResult } from '../components/cover'
import { KeyRound } from '../components/icons'
import { DOMESTIC_HINT, JobSteps, jobLine } from '../components/job'
import { useJobs } from '../components/jobs'
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
import { DOMAIN, formatCount, streamsLeft, validUserName } from '../lib/format'
import { useNow } from '../hooks/usePoll'
import { Button, Dialog, Disclosure, ErrorBox, Field, Input, useToast } from '../components/ui'

type Stage = 'form' | 'hostkey' | 'installing'

const EMPTY = { host: '', name: '', coverDomain: '', firstUser: 'user1', hubPort: '8443', capturePort: '9443' }

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

/** A port for the link or the capture: 1024–65535, and not the SSH port. */
const badPort = (p: number, ssh: number) => !Number.isInteger(p) || p < 1024 || p > 65535 || p === ssh || p === 22

function AddServer({ config, onClose, onInstalled, onOpenServer }: Props) {
  const toast = useToast()
  const jobs = useJobs()
  const now = useNow()
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
  // null follows the default, which may only arrive after the dialog opened
  const [typedStreams, setTypedStreams] = useState<number | null>(null)
  const room = (c: AppConfig | null) => {
    const left = c ? Math.max(0, streamsLeft(c)) : null
    const most = Math.max(100, Math.min(5000, left ?? 5000))
    return { left, most, streams: typedStreams ?? Math.min(c?.defaultMaxStreams ?? 2000, most) }
  }
  const { left, most, streams } = room(config)
  const job = jobs.get(jobId)

  const set = (k: keyof typeof EMPTY) => (e: { target: { value: string } }) => setForm((f) => ({ ...f, [k]: e.target.value }))

  const request = (fingerprint: string, maxStreams: number): InstallRequest => ({
    ...toLogin(login, fingerprint),
    host: form.host.trim(),
    name,
    coverDomain,
    maxStreams,
    firstUser: form.firstUser.trim() || undefined,
    hubPort: Number(form.hubPort),
    capturePort: Number(form.capturePort),
  })

  const install = async (fingerprint: string, maxStreams = streams) => {
    setBusy(true)
    setTesting(true)
    setError(null)
    setCover(null)
    try {
      const { jobId } = await api.install(request(fingerprint, maxStreams))
      setLogin(withoutSecrets)
      setJobId(jobId)
      setStage('installing')
      // reported here even when the dialog is closed by then
      const added = name
      jobs.watch(jobId, (j) => {
        if (j.state === 'ok') toast(`${added} added`)
        else toast('Installation failed', 'error')
        if (j.serverId) onInstalled()
      })
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
    const ssh = Number(login.sshPort)
    const hub = Number(form.hubPort)
    const capture = Number(form.capturePort)
    // a quick submit can beat the router's numbers; the limit follows them
    const now = room(config ?? (await api.config().catch(() => null)))
    const { left } = now
    const problem =
      loginError(login) ??
      (!DOMAIN.test(form.coverDomain.trim())
        ? 'Give the cover site as a domain name, like www.example.com.'
        : first && !validUserName(first)
          ? "Name the first user with letters, digits, '.', '_' or '-'."
          : left !== null && left < 100
            ? "This router has no connections left; lower another server's limit first."
            : badPort(hub, ssh) || badPort(capture, ssh)
              ? 'Ports run from 1024 to 65535 and must differ from the SSH port.'
              : hub === capture
                ? 'The link port and the capture port must differ.'
                : null)
    if (problem) return setError(problem)
    lastUser = login.sshUser.trim()
    setBusy(true)
    setError(null)
    try {
      const probe = await api.probe({ host: form.host.trim(), sshPort: ssh, sshUser: login.sshUser.trim() })
      setKey(probe)
      setAckChanged(false)
      // a first key is pinned as it is, like ssh does; only a changed one stops to ask
      if (!keyNeedsTrust(probe)) {
        setBusy(false)
        await install(probe.fingerprint, now.streams)
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
          variant="danger"
          disabled={!key || !keyTrusted(key, ackChanged)}
          busy={busy}
          onClick={() => key && install(key.fingerprint)}
        >
          {testing ? 'Testing the cover site…' : 'Trust and install'}
        </Button>
      </>
    ) : finished && job.serverId ? (
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
    ) : (
      <Button onClick={onClose}>Close</Button>
    )

  return (
    <Dialog
      // once the test passes the install starts, so it can't be walked away from mid-test
      onClose={testing ? () => {} : onClose}
      closeLabel={stage === 'installing' && !finished ? 'Close; the install keeps going' : 'Close'}
      title={
        stage === 'installing'
          ? job?.state === 'ok'
            ? 'Server added'
            : job?.state === 'failed'
              ? 'Installation failed'
              : `Installing ${name}`
          : 'Add a server'
      }
      description={
        stage === 'form'
          ? 'The login is used once for the install and never stored.'
          : stage === 'installing' && job
            ? jobLine(job, now)
            : undefined
      }
      footer={footer}
    >
      {stage === 'form' && (
        <form id="add-server" onSubmit={submit} className="grid grid-cols-6 gap-3" autoComplete="off">
          <LoginFields
            form={login}
            onChange={setLogin}
            host={
              <Field label="Server address" className="col-span-4">
                  {(p) => (
                    <Input
                      {...p}
                      aria-describedby="domestic-hint"
                      required
                      value={form.host}
                      onChange={set('host')}
                      placeholder="203.0.113.10"
                      spellCheck={false}
                      autoCapitalize="off"
                    />
                  )}
              </Field>
            }
            afterHost={
              <p id="domestic-hint" className="col-span-6 -mt-1 text-xs text-faint">
                {DOMESTIC_HINT}
              </p>
            }
          />
          <Field
            label="Cover site"
            hint="A real HTTPS site with TLS 1.3. The server tests it before installing."
            error={coverFailed && `${cover!.domain} doesn't work as a cover site from this server. Try another.`}
            className="col-span-6"
          >
            {(p) => (
              <Input {...p} required value={form.coverDomain} onChange={set('coverDomain')} spellCheck={false} autoCapitalize="off" placeholder="www.example.com" />
            )}
          </Field>
          {coverFailed && (
            <div className="col-span-6">
              <CheckResult check={coverFailed} domain={cover!.domain} />
            </div>
          )}
          <Field label="Name" className="col-span-3">
            {(p) => <Input {...p} value={form.name} onChange={set('name')} placeholder={form.host || 'Frankfurt VPS'} maxLength={100} />}
          </Field>
          <Field label="First user" className="col-span-3">
            {(p) => <Input {...p} value={form.firstUser} onChange={set('firstUser')} spellCheck={false} autoCapitalize="off" maxLength={32} />}
          </Field>
          <Disclosure title="Advanced" className="col-span-6 border-t border-border pt-3">
            <div className="grid grid-cols-2 gap-3">
              <Field
                label={
                  <span className="flex justify-between">
                    Connections <b className="tabular text-foreground">{formatCount(streams)}</b>
                  </span>
                }
                hint={`How many Telegram connections this server can hold at once.${left !== null ? ` ${formatCount(left)} free on this router.` : ''}`}
                className="col-span-2"
              >
                {(p) => (
                  <input
                    {...p}
                    type="range"
                    min={100}
                    max={most}
                    step={100}
                    value={streams}
                    onChange={(e) => setTypedStreams(Number(e.target.value))}
                    className="h-11 w-full sm:h-6"
                  />
                )}
              </Field>
              <Field label="Link port">
                {(p) => <Input {...p} inputMode="numeric" value={form.hubPort} onChange={set('hubPort')} aria-describedby="port-hint" />}
              </Field>
              <Field label="Capture port">
                {(p) => <Input {...p} inputMode="numeric" value={form.capturePort} onChange={set('capturePort')} />}
              </Field>
              <p id="port-hint" className="col-span-2 -mt-1 text-xs text-faint">
                The link port must be open on the server's firewall.
              </p>
            </div>
          </Disclosure>
          {error && (
            <div className="col-span-6">
              <ErrorBox>{error}</ErrorBox>
            </div>
          )}
          <p className="col-span-6 flex items-start gap-2 text-xs text-faint">
            <KeyRound className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            The server's SSH key is saved on first connect. You're warned if it ever changes.
          </p>
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
          {job?.state === 'ok' && <p className="text-[13px] text-muted">The agent is running and its link is up.</p>}
          {job?.state === 'failed' && job.serverId ? (
            <p className="text-[13px] text-muted">The server is saved. Fix the cause, then run a repair from its page.</p>
          ) : null}
          {!finished && (
            <p className="border-t border-border pt-3 text-[13px] text-muted">
              You can close this. The install keeps going, and the job tray at the top shows it until it's done.
            </p>
          )}
        </div>
      )}
    </Dialog>
  )
}
