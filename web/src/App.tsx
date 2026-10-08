import { useState } from 'react'
import { Moon, Sun, TelegramMtprotoLogo } from './components/icons'
import { api } from './lib/api'
import { usePoll } from './hooks/usePoll'
import { navigate, useRoute } from './hooks/useRoute'
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
              navigate({ page: 'servers' })
            }}
            className="flex items-center gap-2.5 font-semibold"
          >
            <TelegramMtprotoLogo size={30} />
            MTProto Remote
          </a>
          <span className="hidden text-xs text-faint sm:inline">
            Proxy servers that reach Telegram through this router
          </span>
          <div className="ml-auto flex items-center gap-1">
            {config.data && <span className="mr-2 text-xs text-faint">v{config.data.version}</span>}
            <ThemeToggle />
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        {route.page === 'servers' ? (
          <ServersScreen config={config.data} onChanged={reloadConfig} />
        ) : (
          <ServerScreen
            key={route.id}
            id={route.id}
            tab={route.tab}
            config={config.data}
            onConfigChanged={reloadConfig}
          />
        )}
      </main>
    </div>
  )
}
