import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { JobsProvider } from './components/jobs'
import { ToastProvider } from './components/ui'
import './index.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ToastProvider>
      <JobsProvider>
        <App />
      </JobsProvider>
    </ToastProvider>
  </StrictMode>,
)
