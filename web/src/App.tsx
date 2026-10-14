import { useEffect, useState } from 'react'
import { Moon, Sun, TelegramMtprotoLogo } from './components/icons'
import { JobTray } from './components/jobs'
import { api } from './lib/api'
import { usePoll } from './hooks/usePoll'
import { navigate, onLeave, useRoute } from './hooks/useRoute'
import { Button, Confirm } from './components/ui'
import { ServersScreen } from './screens/ServersScreen'
import { ServerScreen } from './screens/ServerScreen'
import { LocalScreen } from './screens/LocalScreen'

function ThemeToggle() {
  const [theme, setTheme] = useState(() => document.documentElement.dataset.theme ?? 'dark')
  const toggle = () => {
    const next = theme === 'dark' ? 'light' : 'dark'
    document.documentElement.dataset.theme = next
    try {
      localStorage.setItem('theme', next)
    } catch {
      // Storage may be blocked; the choice then lasts for this page view.
    }
    setTheme(next)
  }
  return (
    <Button variant="ghost" size="icon" onClick={toggle} aria-label="Toggle light or dark theme">
      {theme === 'dark' ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
    </Button>
  )
}

/** Asks before leaving a page with unsaved changes. */
function LeaveQuestion() {
  const [ask, setAsk] = useState<{ message: string; go: () => void } | null>(null)
  useEffect(() => onLeave((message, go) => setAsk({ message, go })), [])
  if (!ask) return null
  return (
    <Confirm title="Leave without saving?" action="Discard and leave" danger onConfirm={ask.go} onClose={() => setAsk(null)}>
      <p>{ask.message}</p>
    </Confirm>
  )
}

export function App() {
  const route = useRoute()
  const config = usePoll(api.config, 30000)
  const reloadConfig = () => void config.reload()

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-border bg-surface">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-3 px-4">
          {/* "#/" alone resolves against <base href> and reloads the page when the panel's URL has no trailing slash */}
          <a
            href="#/"
            onClick={(e) => {
              e.preventDefault()
              navigate(__LOCAL__ ? { page: 'local', tab: 'users' } : { page: 'servers' })
            }}
            className="flex shrink-0 items-center gap-2.5 font-semibold"
          >
            <TelegramMtprotoLogo size={30} />
            {__LOCAL__ ? 'Local MTProto' : 'Remote MTProto'}
          </a>
          <span className="hidden truncate text-xs text-faint md:inline">
            {__LOCAL__ ? 'A Telegram proxy that runs on this router' : 'Proxy servers that reach Telegram through this router'}
          </span>
          <div className="ml-auto flex items-center gap-1 sm:gap-2">
            {!__LOCAL__ && <JobTray />}
            {config.data && <span className="hidden text-xs text-faint sm:inline">v{config.data.version}</span>}
            <ThemeToggle />
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        {__LOCAL__ ? (
          <LocalScreen tab={route.page === 'servers' ? 'users' : route.tab} />
        ) : route.page === 'servers' ? (
          <ServersScreen config={config.data} onChanged={reloadConfig} />
        ) : route.page === 'server' ? (
          <ServerScreen key={route.id} id={route.id} tab={route.tab} config={config.data} onConfigChanged={reloadConfig} />
        ) : null}
      </main>
      <LeaveQuestion />
    </div>
  )
}
