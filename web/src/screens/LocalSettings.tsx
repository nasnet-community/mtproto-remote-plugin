import { useEffect, useState, type ReactNode } from 'react'
import { RefreshCw, TriangleAlert } from '../components/icons'
import { asServer, local, type LocalProxy } from '../lib/api'
import { DOMAIN, dropLine, formatCount, openConnections, plural } from '../lib/format'
import { setLeaveGuard } from '../hooks/useRoute'
import { Button, Card, Confirm, ErrorBox, Field, Impact, Input, useToast } from '../components/ui'
import { AdvancedCard, CoverCard, Head, Row } from './SettingsTab'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

function TriangleNote({ children }: { children: ReactNode }) {
  return (
    <div className="flex gap-3 rounded-md bg-warn-tint p-3 text-[13px]">
      <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-warn-text" />
      <span>{children}</span>
    </div>
  )
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}$/

function ConnectionsCard({ proxy: p, onChanged }: { proxy: LocalProxy; onChanged: () => void }) {
  const toast = useToast()
  const c = p.maxConns
  const [value, setValue] = useState(c.value)
  const [asking, setAsking] = useState(false)
  const dirty = value !== c.value
  const server = asServer(p)

  useEffect(() => {
    setLeaveGuard('connections', dirty ? `The connection limit (${formatCount(c.value)} → ${formatCount(value)}) isn't saved.` : null)
    return () => setLeaveGuard('connections', null)
  }, [dirty, value, c.value])

  return (
    <Card className="flex flex-col gap-3 p-4">
      <Head title="Connections" hint="How many Telegram connections the proxy holds at once. Each phone opens a few.">
        {dirty && (
          <span className="rounded-full bg-warn-tint px-2.5 py-0.5 text-xs font-medium text-warn-text">
            Unsaved: {formatCount(c.value)} → {formatCount(value)}
          </span>
        )}
      </Head>
      <Field
        label={
          <span className="flex justify-between text-foreground">
            Connection limit <b className="tabular">{formatCount(value)}</b>
          </span>
        }
      >
        {(f) => (
          <div>
            <input
              {...f}
              type="range"
              min={c.min}
              max={c.max}
              step={100}
              value={value}
              onChange={(e) => setValue(Number(e.target.value))}
              className="h-11 w-full sm:h-6"
            />
            <div className="flex justify-between text-xs text-faint">
              <span>{formatCount(c.min)}</span>
              <span>{formatCount(c.max)}</span>
            </div>
          </div>
        )}
      </Field>
      <div className="flex flex-col gap-2 border-t border-border pt-3 sm:flex-row sm:items-center sm:justify-between">
        <span className="text-xs text-muted">
          {formatCount(c.default)} suits this router. Higher limits use more of its memory. Saving restarts the proxy.
        </span>
        <div className="grid grid-flow-col auto-cols-fr gap-2 sm:flex">
          {dirty && (
            <Button size="sm" onClick={() => setValue(c.value)}>
              Discard
            </Button>
          )}
          {!dirty && c.value !== c.default && (
            <Button size="sm" onClick={() => setValue(c.default)}>
              Use {formatCount(c.default)}
            </Button>
          )}
          <Button size="sm" variant="primary" disabled={!dirty} onClick={() => setAsking(true)}>
            Save…
          </Button>
        </div>
      </div>
      {asking && (
        <Confirm
          title="Save the connection limit?"
          action="Save and restart"
          onClose={() => setAsking(false)}
          onConfirm={async () => {
            const saved = await local.settings({ maxConns: value === c.default ? 0 : value })
            toast(`Connection limit set to ${formatCount(saved.maxConns.value)}`)
            onChanged()
          }}
        >
          <Impact n={openConnections(server)}>{dropLine(server)}</Impact>
          <p>The proxy can then hold {formatCount(value)} connections at once.</p>
        </Confirm>
      )}
    </Card>
  )
}

/** Where links point: the router's IP Cloud name and its port, unless set by hand. */
function AddressCard({ proxy: p, onChanged }: { proxy: LocalProxy; onChanged: () => void }) {
  const toast = useToast()
  const a = p.address
  const [host, setHost] = useState(a.customHost)
  const [port, setPort] = useState(a.customPort ? String(a.customPort) : '')
  const [asking, setAsking] = useState<'save' | 'port' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const wantHost = host.trim().toLowerCase()
  const wantPort = port.trim() ? Number(port) : 0
  const hostProblem = wantHost && !DOMAIN.test(wantHost) && !IPV4.test(wantHost) ? 'Give a domain name or an IPv4 address.' : null
  const portProblem = port.trim() && (!Number.isInteger(wantPort) || wantPort < 1 || wantPort > 65535) ? 'Use a port from 1 to 65535.' : null
  const dirty = wantHost !== a.customHost || wantPort !== a.customPort
  const next = `${wantHost || a.cloudName || a.publicAddress || '?'}:${wantPort || a.routerPort || '?'}`
  const users = plural(p.userCount, 'user')

  useEffect(() => {
    setLeaveGuard('address', dirty ? "The address in links isn't saved." : null)
    return () => setLeaveGuard('address', null)
  }, [dirty])

  const save = async () => {
    setError(null)
    try {
      const saved = await local.settings({ linkHost: wantHost, linkPort: wantPort })
      toast(`Links now point at ${saved.address.host || '?'}:${saved.address.port || '?'}; share them again`)
      onChanged()
    } catch (e) {
      setError(message(e))
      throw e
    }
  }

  return (
    <Card className="flex flex-col gap-3 p-4">
      <Head title="Address in links" hint="Where phones connect. It follows the router's IP Cloud name and port unless you set them.">
        <Button size="sm" disabled={!a.reported || !a.domestic || !!a.newPortAskedAt} onClick={() => setAsking('port')}>
          <RefreshCw className="h-3.5 w-3.5" /> New port…
        </Button>
      </Head>
      <div className="grid gap-3 sm:grid-cols-[1fr_10rem]">
        <Field
          label="Address"
          error={hostProblem}
          hint={a.cloudName ? `Empty uses the IP Cloud name, ${a.cloudName}.` : 'IP Cloud has no name for this router: type a name or address that points at it.'}
        >
          {(f) => <Input {...f} value={host} onChange={(e) => setHost(e.target.value)} spellCheck={false} autoCapitalize="off" placeholder={a.cloudName || 'home.example.com'} />}
        </Field>
        <Field label="Port" error={portProblem} hint={`Empty uses the router's, ${a.routerPort || 'once it has one'}.`}>
          {(f) => <Input {...f} inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} placeholder={a.routerPort ? String(a.routerPort) : ''} />}
        </Field>
      </div>
      <p className="text-xs text-faint">Set a port only when your modem forwards a different outside port to the router.</p>
      {error && <ErrorBox>{error}</ErrorBox>}
      <div className="flex flex-col gap-2 border-t border-border pt-3 sm:flex-row sm:items-center sm:justify-between">
        <span className="text-xs text-muted">Links point at {a.host && a.port ? `${a.host}:${a.port}` : 'nothing yet'}.</span>
        <div className="grid grid-flow-col auto-cols-fr gap-2 sm:flex">
          {dirty && (
            <Button
              size="sm"
              onClick={() => {
                setHost(a.customHost)
                setPort(a.customPort ? String(a.customPort) : '')
              }}
            >
              Discard
            </Button>
          )}
          <Button size="sm" variant="primary" disabled={!dirty || !!hostProblem || !!portProblem} onClick={() => (p.userCount ? setAsking('save') : void save().catch(() => {}))}>
            Save…
          </Button>
        </div>
      </div>
      {asking === 'save' && (
        <Confirm title="Change the address in links?" action="Change links" onClose={() => setAsking(null)} onConfirm={save}>
          <TriangleNote>
            <b>All {users} get new links,</b> pointing at {next}. Their old links stop working, so you'll need to share the new ones.
          </TriangleNote>
        </Confirm>
      )}
      {asking === 'port' && (
        <Confirm
          title="Move the proxy to a new port?"
          action="Pick a new port"
          onClose={() => setAsking(null)}
          onConfirm={async () => {
            await local.newPort()
            toast('The router moves to a new port within a minute')
            onChanged()
          }}
        >
          <TriangleNote>
            <b>All {users} get new links,</b> and the forward on your modem has to move to the new port too. Do this when the port stops working.
          </TriangleNote>
          <p>The router picks a free port at random and moves the forward within a minute.</p>
        </Confirm>
      )}
    </Card>
  )
}

export function LocalSettings({ proxy: p, onChanged, onChangeCover }: { proxy: LocalProxy; onChanged: () => void; onChangeCover: () => void }) {
  const server = asServer(p)
  return (
    <div className="flex flex-col gap-4">
      <CoverCard server={server} onChange={onChangeCover} />
      <AddressCard key={`${p.address.customHost}:${p.address.customPort}`} proxy={p} onChanged={onChanged} />
      <ConnectionsCard key={p.maxConns.value} proxy={p} onChanged={onChanged} />
      <AdvancedCard server={server} onChanged={onChanged} />
      <Card className="p-4">
        <h2 className="font-semibold">About</h2>
        <dl className="mt-2 divide-y divide-border">
          <Row label="Local MTProto">{p.version}</Row>
          <Row label="Proxy">
            telemt {p.proxy.version || '–'}
            {p.proxy.build && <span className="block text-xs text-faint">{p.proxy.build}</span>}
          </Row>
          <Row label="Architecture">{p.arch}</Row>
          <Row label="Set up">
            {new Date(p.createdAt).toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' })}
          </Row>
        </dl>
        <p className="mt-2 text-xs text-faint">
          The proxy is telemt, made by its authors at github.com/telemt/telemt and used under its licence. Local MTProto configures and runs it.
        </p>
      </Card>
    </div>
  )
}
