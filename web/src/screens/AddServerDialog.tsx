import { useEffect, useState, type FormEvent } from 'react'
import { AlertTriangle, CheckCircle2, Circle, KeyRound, XCircle } from 'lucide-react'
import { api, type InstallRequest, type Job, type ProbeResult } from '../lib/api'
import { USERNAME } from '../lib/format'
import { Button, Dialog, ErrorBox, Field, Input, Spinner, cx } from '../components/ui'

type Stage = 'form' | 'hostkey' | 'installing'

const EMPTY = {
  host: '',
  sshPort: '22',
  username: 'root',
  password: '',
  name: '',
  proxyPort: '443',
  tlsDomain: '',
  firstUser: 'user1',
}

function Progress({ job }: { job: Job }) {
  return (
    <ol className="flex flex-col gap-3">
      {job.steps.map((step, i) => (
        <li key={i} className="flex gap-3">
          <span className="mt-0.5">
            {step.state === 'running' && <Spinner className="text-accent" />}
            {step.state === 'done' && <CheckCircle2 className="h-4 w-4 text-status-online" />}
            {step.state === 'failed' && <XCircle className="h-4 w-4 text-status-danger" />}
          </span>
          <div className="min-w-0">
            <div className={cx(step.state === 'running' && 'font-medium')}>{step.title}</div>
            {step.detail && (
              <div
                className={cx(
                  'whitespace-pre-wrap break-words text-xs',
                  step.state === 'failed' ? 'text-status-danger' : 'text-muted',
                )}
              >
                {step.detail}
              </div>
            )}
          </div>
        </li>
      ))}
      {job.running && job.steps.length === 0 && (
        <li className="flex gap-3 text-muted">
          <Circle className="h-4 w-4" /> Starting…
        </li>
      )}
    </ol>
  )
}

export function AddServerDialog({
  open,
  onClose,
  onInstalled,
}: {
  open: boolean
  onClose: () => void
  onInstalled: (serverId: string) => void
}) {
  const [form, setForm] = useState(EMPTY)
  const [stage, setStage] = useState<Stage>('form')
  const [key, setKey] = useState<ProbeResult | null>(null)
  const [ackChanged, setAckChanged] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [job, setJob] = useState<Job | null>(null)

  useEffect(() => {
    if (open) {
      setStage('form')
      setError(null)
      setJob(null)
      setKey(null)
      setForm((f) => ({ ...EMPTY, username: f.username || 'root' }))
    }
  }, [open])

  // Poll the install job while it runs.
  useEffect(() => {
    if (stage !== 'installing') return
    let stop = false
    const tick = async () => {
      try {
        const j = await api.job()
        if (stop) return
        setJob(j)
        if (j.running) setTimeout(tick, 800)
      } catch {
        if (!stop) setTimeout(tick, 2000)
      }
    }
    void tick()
    return () => {
      stop = true
    }
  }, [stage])

  const set = (k: keyof typeof EMPTY) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: e.target.value }))

  const request = (fingerprint: string): InstallRequest => ({
    host: form.host.trim(),
    sshPort: Number(form.sshPort),
    username: form.username.trim(),
    password: form.password,
    fingerprint,
    name: form.name.trim(),
    proxyPort: Number(form.proxyPort),
    tlsDomain: form.tlsDomain.trim(),
    firstUser: form.firstUser.trim(),
  })

  const install = async (fingerprint: string) => {
    setBusy(true)
    setError(null)
    try {
      await api.install(request(fingerprint))
      setForm((f) => ({ ...f, password: '' }))
      setStage('installing')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setStage('form')
    } finally {
      setBusy(false)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!USERNAME.test(form.firstUser.trim())) {
      setError("Name the first user with letters, digits, '.', '_' or '-'.")
      return
    }
    setBusy(true)
    setError(null)
    try {
      const probe = await api.probe(form.host.trim(), Number(form.sshPort))
      setKey(probe)
      setAckChanged(false)
      if (probe.status === 'known') {
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

  const finished = job && !job.running
  const serverId = job?.report?.serverId

  const footer =
    stage === 'form' ? (
      <>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" type="submit" form="add-server" busy={busy}>
          Connect and install
        </Button>
      </>
    ) : stage === 'hostkey' ? (
      <>
        <Button onClick={() => setStage('form')}>Back</Button>
        <Button
          variant={key?.status === 'changed' ? 'danger' : 'primary'}
          disabled={key?.status === 'changed' && !ackChanged}
          busy={busy}
          onClick={() => key && install(key.fingerprint)}
        >
          Trust and install
        </Button>
      </>
    ) : finished && serverId && !job.error ? (
      <Button variant="primary" onClick={() => onInstalled(serverId)}>
        Open server
      </Button>
    ) : finished ? (
      <>
        <Button onClick={onClose}>Close</Button>
        <Button variant="primary" onClick={() => setStage('form')}>
          Try again
        </Button>
      </>
    ) : null

  return (
    <Dialog
      open={open}
      onClose={job?.running ? () => {} : onClose}
      title={
        stage === 'installing'
          ? job?.running === false
            ? job.error
              ? 'Installation failed'
              : 'Server added'
            : 'Installing telemt'
          : 'Add a server'
      }
      description={
        stage === 'form'
          ? 'The router logs in once with this password, installs telemt, and from then on uses its own SSH key.'
          : undefined
      }
      footer={footer}
    >
      {stage === 'form' && (
        <form id="add-server" onSubmit={submit} className="grid grid-cols-6 gap-3" autoComplete="off">
          <Field label="Server address" className="col-span-4">
            {(id) => (
              <Input id={id} required value={form.host} onChange={set('host')} placeholder="203.0.113.10" spellCheck={false} />
            )}
          </Field>
          <Field label="SSH port" className="col-span-2">
            {(id) => <Input id={id} required type="number" min={1} max={65535} value={form.sshPort} onChange={set('sshPort')} />}
          </Field>
          <Field label="Username" className="col-span-3">
            {(id) => <Input id={id} required value={form.username} onChange={set('username')} spellCheck={false} autoCapitalize="off" />}
          </Field>
          <Field label="Password" className="col-span-3">
            {(id) => <Input id={id} required type="password" value={form.password} onChange={set('password')} autoComplete="new-password" />}
          </Field>
          <Field label="Name" hint="Optional; shown in this list." className="col-span-6">
            {(id) => <Input id={id} value={form.name} onChange={set('name')} placeholder={form.host || 'Frankfurt VPS'} maxLength={64} />}
          </Field>
          <Field label="Proxy port" className="col-span-2">
            {(id) => <Input id={id} required type="number" min={1} max={65535} value={form.proxyPort} onChange={set('proxyPort')} />}
          </Field>
          <Field
            label="Disguise as"
            hint="A real HTTPS site; traffic looks like visits to it."
            className="col-span-4"
          >
            {(id) => <Input id={id} required value={form.tlsDomain} onChange={set('tlsDomain')} placeholder="www.google.com" spellCheck={false} />}
          </Field>
          <Field label="First user" hint="Add more after install." className="col-span-6">
            {(id) => <Input id={id} required value={form.firstUser} onChange={set('firstUser')} spellCheck={false} maxLength={64} />}
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
          {key.status === 'changed' ? (
            <div className="flex gap-3 rounded-md border border-status-warn p-3 tint-warn">
              <AlertTriangle className="h-5 w-5 shrink-0 text-status-warn" />
              <p className="text-[13px]">
                <b>{form.host}</b> shows a different key than last time. That happens after a
                reinstall — or when someone is intercepting the connection.
              </p>
            </div>
          ) : (
            <p className="text-[13px] text-muted">
              First connection to <b className="text-foreground">{form.host}</b>. Check the
              fingerprint matches the server:{' '}
              <code className="text-xs">ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code>
            </p>
          )}
          <div className="flex items-center gap-2 rounded-md bg-card2 p-3 font-mono text-xs break-all">
            <KeyRound className="h-4 w-4 shrink-0 text-muted" />
            {key.fingerprint}
          </div>
          {key.status === 'changed' && (
            <label className="flex items-center gap-2 text-[13px]">
              <input type="checkbox" checked={ackChanged} onChange={(e) => setAckChanged(e.target.checked)} />
              I know why the key changed
            </label>
          )}
          {error && <ErrorBox>{error}</ErrorBox>}
        </div>
      )}

      {stage === 'installing' && (
        <div className="flex flex-col gap-4">
          {job ? <Progress job={job} /> : <Spinner />}
          {job?.error && <ErrorBox>{job.error}</ErrorBox>}
          {finished && job.report && !job.error && (
            <p className="text-[13px] text-muted">
              telemt {job.report.release} is running with {job.report.users} user
              {job.report.users === 1 ? '' : 's'}
              {job.report.keptConfig ? ' (existing configuration kept)' : ''}.
            </p>
          )}
        </div>
      )}
    </Dialog>
  )
}
