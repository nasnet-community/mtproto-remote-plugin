import type { Config } from 'tailwindcss'

// Same tokens as nasnet-monitor, so the plugins look alike in the panel.
export default {
  darkMode: ['selector', '[data-theme="dark"]'],
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        background: 'var(--bg)',
        foreground: 'var(--text)',
        surface: 'var(--surface)',
        card: 'var(--card)',
        card2: 'var(--card2)',
        border: 'var(--border)',
        'border-strong': 'var(--border-strong)',
        muted: 'var(--muted)',
        faint: 'var(--faint)',
        track: 'var(--track)',
        accent: 'var(--accent)',
        'accent-fg': 'var(--accent-fg)',
        status: {
          online: 'var(--status-online)',
          warn: 'var(--status-warn)',
          danger: 'var(--status-danger)',
          idle: 'var(--status-idle)',
        },
      },
      borderRadius: { lg: '0.85rem', md: '0.6rem', sm: '0.4rem' },
      fontFamily: {
        sans: ['Geist', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['"Geist Mono"', 'ui-monospace', 'monospace'],
      },
    },
  },
} satisfies Config
