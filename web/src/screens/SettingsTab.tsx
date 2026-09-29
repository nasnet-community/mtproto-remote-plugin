import { useState } from 'react'
import { TriangleAlert } from '../components/icons'
import { users as usersApi, type AppConfig, type Server } from '../lib/api'
import { Button, Card, CopyButton, ErrorBox, Field, Input, useToast } from '../components/ui'

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-1 py-2.5 sm:grid-cols-[180px_1fr] sm:gap-4">
      <dt className="text-[13px] text-muted">{label}</dt>
      <dd className="min-w-0 break-all text-[13px]">{children}</dd>
    </div>
  )
}

function DisguiseCard({ server }: { server: Server }) {
  const toast = useToast()
  const [domain, setDomain] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const valid = /^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$/.test(domain.trim())

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      await usersApi.setTlsDomain(server.id, domain.trim())
      toast(`Now disguised as ${domain.trim()}; share the new links`)
      setDomain('')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="flex flex-col gap-3 p-4">
      <div>
        <h2 className="font-semibold">Disguise domain</h2>
        <p className="text-[13px] text-muted">
          Proxy traffic looks like HTTPS visits to this site. Installed with{' '}
          <b className="text-foreground">{server.tlsDomain}</b>.
        </p>
      </div>
      <div className="flex gap-3 rounded-md border border-status-warn p-3 tint-warn text-[13px]">
        <TriangleAlert className="h-4 w-4 shrink-0 text-status-warn" />
        Changing it changes every user's link: the old links stop working.
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <Field label="New domain" className="min-w-[220px] flex-1">
          {(id) => (
            <Input
              id={id}
              value={domain}
              onChange={(e) => setDomain(e.target.value)}
              placeholder="www.cloudflare.com"
              spellCheck={false}
            />
          )}
        </Field>
        <Button variant="primary" busy={busy} disabled={!valid} onClick={save}>
          Change domain
        </Button>
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
    </Card>
  )
}

export function SettingsTab({
  server,
  config,
  version,
}: {
  server: Server
  config: AppConfig | null
  version?: string
}) {
  return (
    <div className="flex flex-col gap-4">
      <DisguiseCard server={server} />
      <Card className="p-4">
        <h2 className="font-semibold">Server</h2>
        <dl className="mt-2 divide-y divide-border">
          <Row label="SSH">
            <span className="font-mono">
              {server.username}@{server.host}:{server.sshPort}
            </span>
          </Row>
          <Row label="Host key">
            <span className="font-mono text-xs">{server.fingerprint}</span>
          </Row>
          <Row label="Architecture">{server.arch}</Row>
          <Row label="Proxy version">{version ?? '–'}</Row>
          <Row label="Added">{new Date(server.addedAt * 1000).toLocaleString()}</Row>
        </dl>
      </Card>
      {config && (
        <Card className="flex flex-col gap-3 p-4">
          <div>
            <h2 className="font-semibold">Router key</h2>
            <p className="text-[13px] text-muted">
              The router logs in to its servers with this key; it sits in{' '}
              <code className="text-xs">~/.ssh/authorized_keys</code> of{' '}
              <b className="text-foreground">{server.username}</b>. Removing that line cuts the
              router off.
            </p>
          </div>
          <div className="break-all rounded-md bg-card2 p-3 font-mono text-xs">
            {config.routerKey}
          </div>
          <div>
            <CopyButton text={config.routerKey} label="Copy key" />
          </div>
        </Card>
      )}
    </div>
  )
}
