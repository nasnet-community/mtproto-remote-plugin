import type { Config } from 'tailwindcss'

// nasnet-panel's palette and shapes, so the plugin looks at home in the panel.
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
        'accent-strong': 'var(--accent-strong)',
        info: 'var(--info)',
        'accent-fg': 'var(--accent-fg)',
        status: {
          online: 'var(--status-online)',
          warn: 'var(--status-warn)',
          danger: 'var(--status-danger)',
          idle: 'var(--status-idle)',
        },
      },
      borderRadius: { lg: '12px', md: '8px', sm: '4px' },
      boxShadow: { sm: 'var(--shadow-sm)', lg: 'var(--shadow-lg)' },
      fontFamily: {
        sans: ['-apple-system', 'BlinkMacSystemFont', '"Segoe UI"', 'Roboto', 'Helvetica', 'Arial', 'sans-serif'],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
    },
  },
} satisfies Config
