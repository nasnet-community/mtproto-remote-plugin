import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
} from 'react'
import { createPortal } from 'react-dom'
import qrcode from 'qrcode-generator'
import { Check, Copy, Loader2, X } from 'lucide-react'

export function cx(...classes: (string | false | null | undefined)[]) {
  return classes.filter(Boolean).join(' ')
}

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger'

const VARIANTS: Record<Variant, string> = {
  primary: 'bg-accent text-accent-fg hover:brightness-110 border-transparent',
  secondary: 'bg-card2 text-foreground hover:bg-track border-border-strong',
  ghost: 'bg-transparent text-muted hover:text-foreground hover:bg-track border-transparent',
  danger: 'bg-status-danger text-white hover:brightness-110 border-transparent',
}

export function Button({
  variant = 'secondary',
  size = 'md',
  busy = false,
  className,
  children,
  disabled,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant
  size?: 'sm' | 'md' | 'icon'
  busy?: boolean
}) {
  return (
    <button
      type="button"
      disabled={disabled || busy}
      className={cx(
        'inline-flex items-center justify-center gap-2 rounded-md border font-medium transition',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent',
        'disabled:cursor-not-allowed disabled:opacity-50',
        size === 'sm' && 'h-8 px-3 text-[13px]',
        size === 'md' && 'h-9 px-4',
        size === 'icon' && 'h-8 w-8',
        VARIANTS[variant],
        className,
      )}
      {...rest}
    >
      {busy && <Loader2 className="h-4 w-4 animate-spin" />}
      {children}
    </button>
  )
}

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cx(
        'h-9 w-full rounded-md border border-border-strong bg-surface px-3 text-foreground',
        'placeholder:text-faint focus:outline focus:outline-2 focus:outline-accent',
        'disabled:opacity-60',
        className,
      )}
      {...rest}
    />
  )
}

/** A labelled form field; the label is tied to the single input child by id. */
export function Field({
  label,
  hint,
  error,
  children,
  className,
}: {
  label: string
  hint?: ReactNode
  error?: string | null
  children: (id: string) => ReactNode
  className?: string
}) {
  const id = useId()
  return (
    <div className={cx('flex flex-col gap-1.5', className)}>
      <label htmlFor={id} className="text-[13px] font-medium text-muted">
        {label}
      </label>
      {children(id)}
      {error ? (
        <p className="text-xs text-status-danger">{error}</p>
      ) : hint ? (
        <p className="text-xs text-faint">{hint}</p>
      ) : null}
    </div>
  )
}

export function Card({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <div className={cx('rounded-lg border border-border bg-card', className)}>{children}</div>
  )
}

type Tone = 'online' | 'warn' | 'danger' | 'idle'

const TONES: Record<Tone, string> = {
  online: 'text-status-online tint-online',
  warn: 'text-status-warn tint-warn',
  danger: 'text-status-danger tint-danger',
  idle: 'text-status-idle bg-track',
}

export function Badge({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span
      className={cx(
        'inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium',
        TONES[tone],
      )}
    >
      <span className="h-1.5 w-1.5 rounded-full bg-current" />
      {children}
    </span>
  )
}

export function Switch({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  label: string
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        'relative h-5 w-9 shrink-0 rounded-full transition disabled:opacity-50',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent',
        checked ? 'bg-accent' : 'bg-border-strong',
      )}
    >
      <span
        className={cx(
          'absolute top-0.5 h-4 w-4 rounded-full bg-white shadow transition-all',
          checked ? 'left-[18px]' : 'left-0.5',
        )}
      />
    </button>
  )
}

export function Spinner({ className }: { className?: string }) {
  return <Loader2 className={cx('h-4 w-4 animate-spin text-muted', className)} />
}

export function Dialog({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  wide,
}: {
  open: boolean
  onClose: () => void
  title: string
  description?: ReactNode
  children?: ReactNode
  footer?: ReactNode
  wide?: boolean
}) {
  const panel = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    document.addEventListener('keydown', onKey)
    const previous = document.activeElement as HTMLElement | null
    const first = panel.current?.querySelector<HTMLElement>(
      'input:not([disabled]), textarea, select, button:not([data-close])',
    )
    first?.focus()
    return () => {
      document.removeEventListener('keydown', onKey)
      previous?.focus?.()
    }
  }, [open, onClose])

  if (!open) return null
  return createPortal(
    <div className="fixed inset-0 z-40 flex items-end justify-center sm:items-center sm:p-4">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className={cx(
          'relative flex max-h-[92vh] w-full flex-col rounded-t-lg border border-border-strong bg-surface shadow-2xl sm:rounded-lg',
          wide ? 'sm:max-w-3xl' : 'sm:max-w-md',
        )}
      >
        <div className="flex items-start justify-between gap-4 border-b border-border px-5 py-4">
          <div>
            <h2 className="text-base font-semibold">{title}</h2>
            {description && <p className="mt-1 text-[13px] text-muted">{description}</p>}
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close" data-close>
            <X className="h-4 w-4" />
          </Button>
        </div>
        {children && <div className="overflow-y-auto px-5 py-4">{children}</div>}
        {footer && (
          <div className="flex flex-wrap justify-end gap-2 border-t border-border px-5 py-3">
            {footer}
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

export interface MenuItem {
  label: string
  icon?: ReactNode
  danger?: boolean
  onSelect: () => void
}

export function Menu({ trigger, items }: { trigger: ReactNode; items: MenuItem[] }) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="relative" ref={root}>
      <div onClick={() => setOpen((o) => !o)}>{trigger}</div>
      {open && (
        <div
          role="menu"
          className="absolute right-0 z-30 mt-1 min-w-[190px] overflow-hidden rounded-md border border-border-strong bg-surface py-1 shadow-xl"
        >
          {items.map((item) => (
            <button
              key={item.label}
              role="menuitem"
              type="button"
              onClick={() => {
                setOpen(false)
                item.onSelect()
              }}
              className={cx(
                'flex w-full items-center gap-2.5 px-3 py-2 text-left text-[13px] hover:bg-track',
                item.danger ? 'text-status-danger' : 'text-foreground',
              )}
            >
              <span className="text-muted [&>svg]:h-4 [&>svg]:w-4">{item.icon}</span>
              {item.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

interface Toast {
  id: number
  text: string
  tone: 'ok' | 'error'
}

const ToastContext = createContext<(text: string, tone?: Toast['tone']) => void>(() => {})

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const push = useCallback((text: string, tone: Toast['tone'] = 'ok') => {
    const id = Date.now() + Math.random()
    setToasts((t) => [...t, { id, text, tone }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), tone === 'error' ? 7000 : 3500)
  }, [])
  return (
    <ToastContext.Provider value={push}>
      {children}
      <div className="pointer-events-none fixed bottom-4 left-1/2 z-50 flex w-[min(92vw,420px)] -translate-x-1/2 flex-col gap-2">
        {toasts.map((t) => (
          <div
            key={t.id}
            role="status"
            className={cx(
              'pointer-events-auto rounded-md border px-4 py-3 text-[13px] shadow-xl',
              t.tone === 'error'
                ? 'border-danger-soft bg-surface text-status-danger'
                : 'border-border-strong bg-surface text-foreground',
            )}
          >
            {t.text}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export const useToast = () => useContext(ToastContext)

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
    } catch {
      // Plain-HTTP pages have no clipboard API; fall back to a hidden textarea.
      const area = document.createElement('textarea')
      area.value = text
      document.body.append(area)
      area.select()
      document.execCommand('copy')
      area.remove()
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <Button size="sm" onClick={copy} aria-label={label}>
      {copied ? <Check className="h-4 w-4 text-status-online" /> : <Copy className="h-4 w-4" />}
      {copied ? 'Copied' : label}
    </Button>
  )
}

export function QrCode({ text, size = 200 }: { text: string; size?: number }) {
  const svg = useMemo(() => {
    const qr = qrcode(0, 'M')
    qr.addData(text)
    qr.make()
    return qr.createSvgTag({ cellSize: 4, margin: 4, scalable: true })
  }, [text])
  return (
    <div
      className="rounded-md bg-white p-1 [&>svg]:h-full [&>svg]:w-full"
      style={{ width: size, height: size }}
      role="img"
      aria-label="QR code of the proxy link"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  )
}

export function ErrorBox({ children }: { children: ReactNode }) {
  return (
    <div className="whitespace-pre-wrap break-words rounded-md border border-danger-soft tint-danger px-3 py-2 text-[13px] text-status-danger">
      {children}
    </div>
  )
}
