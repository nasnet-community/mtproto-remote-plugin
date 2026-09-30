import { useEffect, useState } from 'react'
import { Moon, Sun, TelegramMtprotoLogo } from './components/icons'
import { api, type AppConfig } from './lib/api'
import { useRoute } from './hooks/useRoute'
import { Button } from './components/ui'
import { ServersScreen } from './screens/ServersScreen'
import { ServerScreen } from './screens/ServerScreen'

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

export function App() {
  const route = useRoute()
  const [config, setConfig] = useState<AppConfig | null>(null)
  useEffect(() => {
    api.config().then(setConfig, () => setConfig(null))
  }, [])

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-border bg-surface">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-3 px-4">
          <a href="#/" className="flex items-center gap-2.5 font-semibold">
            <TelegramMtprotoLogo size={30} />
            MTProto Remote
          </a>
          <span className="hidden text-xs text-faint sm:inline">
            MTProto proxies on your own servers
          </span>
          <div className="ml-auto flex items-center gap-1">
            {config && <span className="mr-2 text-xs text-faint">v{config.version}</span>}
            <ThemeToggle />
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        {route.page === 'servers' ? (
          <ServersScreen />
        ) : (
          <ServerScreen id={route.id} tab={route.tab} config={config} />
        )}
      </main>
    </div>
  )
}
