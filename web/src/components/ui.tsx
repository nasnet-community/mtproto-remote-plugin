import {
  createContext,
  useCallback,
  useContext,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from 'react'
import { createPortal } from 'react-dom'
import qrcode from 'qrcode-generator'
import { Check, ChevronDown, Copy, Info, Loader, TriangleAlert, X } from './icons'

export function cx(...classes: (string | false | null | undefined)[]) {
  return classes.filter(Boolean).join(' ')
}

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger'

// Mirrors nasnet-panel's Button.module.scss variants.
const VARIANTS: Record<Variant, string> = {
  primary: 'bg-accent text-accent-fg border-accent hover:bg-accent-strong',
  secondary: 'bg-card2 text-foreground border-border hover:bg-surface hover:border-border-strong',
  ghost: 'bg-transparent text-foreground border-transparent hover:bg-card2',
  danger: 'bg-bad text-white border-bad hover:brightness-95',
}

// phones get 44 px targets; desktops keep the panel's compact sizes
const SIZES = {
  md: 'h-11 px-4 sm:h-9',
  sm: 'h-11 px-3 text-[13px] sm:h-8',
  icon: 'h-11 w-11 sm:h-8 sm:w-8',
  xs: 'h-8 px-2 text-xs sm:h-6',
}

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant
  size?: keyof typeof SIZES
  busy?: boolean
}

/** A button's classes, for the few buttons that can't be a Button (a menu trigger needs a ref). */
export const btn = (variant: Variant = 'secondary', size: keyof typeof SIZES = 'md', className?: string) =>
  cx(
    'inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-md border font-medium transition',
    'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent',
    'disabled:cursor-not-allowed disabled:opacity-50',
    SIZES[size],
    VARIANTS[variant],
    className,
  )

export function Button({ variant, size, busy = false, className, children, disabled, ...rest }: ButtonProps) {
  return (
    <button type="button" disabled={disabled || busy} className={btn(variant, size, className)} {...rest}>
      {busy && <Loader className="h-4 w-4 animate-spin" />}
      {children}
    </button>
  )
}

const FIELD = 'w-full rounded-md border border-border-strong bg-surface text-foreground placeholder:text-faint focus:outline focus:outline-2 focus:outline-accent disabled:opacity-60'

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cx(FIELD, 'h-11 px-3 sm:h-9', className)} {...rest} />
}

export function Textarea({ className, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cx(FIELD, 'min-h-[88px] px-3 py-2 font-mono text-xs', className)} {...rest} />
}

export function Select({ className, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cx(FIELD, 'h-11 px-3 sm:h-9', className)} {...rest} />
}

/** What a Field hands its control, so the label, hint and error are tied to it. */
export interface FieldProps {
  id: string
  'aria-describedby'?: string
  'aria-invalid'?: true
}

/** A labelled form field; the label, hint and error are tied to the one control it renders. */
export function Field({
  label,
  hint,
  error,
  children,
  className,
}: {
  label: ReactNode
  hint?: ReactNode
  error?: string | null | false
  children: (p: FieldProps) => ReactNode
  className?: string
}) {
  const id = useId()
  const note = error || hint
  return (
    <div className={cx('flex flex-col gap-1.5', className)}>
      <label htmlFor={id} className="text-[13px] font-medium text-muted">
        {label}
      </label>
      {children({ id, 'aria-describedby': note ? `${id}-n` : undefined, 'aria-invalid': error ? true : undefined })}
      {note && (
        <p id={`${id}-n`} className={cx('text-xs', error ? 'text-bad-text' : 'text-faint')}>
          {note}
        </p>
      )}
    </div>
  )
}

export function Card({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cx('rounded-lg border border-border bg-card shadow-sm', className)}>{children}</div>
}

export type Tone = 'online' | 'warn' | 'danger' | 'idle' | 'active' | 'muted'

const TONES: Record<Tone, [string, string]> = {
  online: ['bg-ok-tint text-ok-text', 'bg-ok'],
  warn: ['bg-warn-tint text-warn-text', 'bg-warn'],
  danger: ['bg-bad-tint text-bad-text', 'bg-bad'],
  idle: ['bg-track text-muted', 'bg-faint'],
  // a user who may connect but has no connection now
  active: ['bg-track text-foreground', 'bg-ok opacity-50'],
  muted: ['bg-track text-muted', 'bg-faint'],
}

export function Badge({ tone, children, className }: { tone: Tone; children: ReactNode; className?: string }) {
  return (
    <span
      className={cx(
        'inline-flex w-fit items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium',
        TONES[tone][0],
        className,
      )}
    >
      <span className={cx('h-1.5 w-1.5 shrink-0 rounded-full', TONES[tone][1])} />
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
      className="group -my-3 flex h-11 shrink-0 items-center rounded-full focus-visible:outline-none disabled:opacity-50 sm:my-0 sm:h-5"
    >
      <span
        className={cx(
          'relative h-6 w-10 rounded-full transition sm:h-5 sm:w-9',
          'group-focus-visible:outline group-focus-visible:outline-2 group-focus-visible:outline-offset-2 group-focus-visible:outline-accent',
          checked ? 'bg-ok' : 'bg-border-strong',
        )}
      >
        <span
          className={cx(
            'absolute top-0.5 h-5 w-5 rounded-full bg-white shadow transition-all sm:h-4 sm:w-4',
            checked ? 'left-[18px]' : 'left-0.5',
          )}
        />
      </span>
    </button>
  )
}

export function Spinner({ className }: { className?: string }) {
  return <Loader className={cx('h-4 w-4 animate-spin text-muted', className)} />
}

/** A thin bar filled to `pct`. */
export function Bar({ pct, className, fill = 'bg-info' }: { pct: number; className?: string; fill?: string }) {
  return (
    <span className={cx('relative block h-1.5 overflow-hidden rounded-full bg-track', className)}>
      <span className={cx('absolute inset-y-0 left-0 rounded-full', fill)} style={{ width: `${Math.max(2, Math.min(100, pct))}%` }} />
    </span>
  )
}

// --- dialogs -------------------------------------------------------------------

const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])'
// open dialogs, innermost last: only it answers Escape and Tab
const stack: object[] = []

export function Dialog({
  open = true,
  onClose,
  title,
  description,
  children,
  footer,
  wide,
  alert,
  closeLabel = 'Close',
}: {
  open?: boolean
  onClose: () => void
  title: string
  description?: ReactNode
  children?: ReactNode
  footer?: ReactNode
  wide?: boolean
  /** a confirmation: screen readers read it at once */
  alert?: boolean
  closeLabel?: string
}) {
  const panel = useRef<HTMLDivElement>(null)
  // a new onClose every parent render must not re-run the focus effect
  const close = useRef(onClose)
  close.current = onClose
  const titleId = useId()
  // set up before the first paint, so a key pressed right away is not lost
  useLayoutEffect(() => {
    if (!open) return
    const me = {}
    stack.push(me)
    const onKey = (e: KeyboardEvent) => {
      if (stack[stack.length - 1] !== me) return
      if (e.key === 'Escape') {
        e.stopPropagation()
        return close.current()
      }
      if (e.key !== 'Tab' || !panel.current) return
      const items = [...panel.current.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => el.offsetParent !== null)
      if (!items.length) return
      const first = items[0]
      const last = items[items.length - 1]
      const at = document.activeElement
      if (!panel.current.contains(at)) {
        e.preventDefault()
        first.focus()
      } else if (e.shiftKey && at === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && at === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    const previous = document.activeElement as HTMLElement | null
    const first = panel.current?.querySelector<HTMLElement>(
      'input:not([disabled]),textarea,select,button:not([data-close]):not([disabled])',
    )
    ;(first ?? panel.current)?.focus()
    return () => {
      document.removeEventListener('keydown', onKey)
      stack.splice(stack.indexOf(me), 1)
      // a trigger that went away (a menu item) can't take it back
      if (previous?.isConnected) previous.focus()
    }
  }, [open])

  if (!open) return null
  return createPortal(
    <div className="fixed inset-0 z-40 flex items-end justify-center sm:items-center sm:p-4">
      <div className="absolute inset-0" style={{ background: 'var(--overlay)' }} onClick={onClose} />
      <div
        ref={panel}
        role={alert ? 'alertdialog' : 'dialog'}
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className={cx(
          'relative flex max-h-[92vh] w-full flex-col rounded-t-2xl border border-border bg-surface shadow-lg outline-none sm:rounded-lg',
          wide ? 'sm:max-w-3xl' : 'sm:max-w-md',
        )}
      >
        <span className="mx-auto mt-2 h-1 w-10 rounded-full bg-border-strong sm:hidden" />
        <div className="flex items-start justify-between gap-4 px-5 pb-3 pt-3 sm:border-b sm:border-border sm:py-4">
          <div className="min-w-0">
            <h2 id={titleId} className="text-lg font-semibold leading-snug sm:text-base">
              {title}
            </h2>
            {description && <p className="mt-1 text-[13px] text-muted">{description}</p>}
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label={closeLabel} data-close className="-mr-2 -mt-1">
            <X className="h-4 w-4" />
          </Button>
        </div>
        {children && <div className="overflow-y-auto px-5 pb-4 sm:pt-4">{children}</div>}
        {footer && (
          <div className="pb-safe grid grid-flow-col auto-cols-fr gap-2 border-t border-border px-5 pt-3 sm:flex sm:justify-end sm:pb-3">
            {footer}
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

/** Asks before something that can't be taken back, or that drops users. */
export function Confirm({
  title,
  children,
  action,
  danger,
  onConfirm,
  onClose,
}: {
  title: string
  children?: ReactNode
  action: string
  danger?: boolean
  /** throws to keep the dialog open with the error */
  onConfirm: () => Promise<unknown> | void
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  return (
    <Dialog
      alert
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant={danger ? 'danger' : 'primary'}
            busy={busy}
            onClick={async () => {
              setBusy(true)
              try {
                await onConfirm()
                onClose()
              } catch (e) {
                setError(e instanceof Error ? e.message : String(e))
                setBusy(false)
              }
            }}
          >
            {action}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-[13px] text-muted">
        {children}
        {error && <ErrorBox>{error}</ErrorBox>}
      </div>
    </Dialog>
  )
}

/** The number a confirmation turns on, big, with what it means. */
export function Impact({ n, children }: { n: number; children: ReactNode }) {
  return (
    <div className={cx('flex items-center gap-3 rounded-md p-3 text-[13px] text-foreground', n ? 'bg-warn-tint' : 'bg-card2')}>
      <span className={cx('tabular text-2xl font-semibold', n ? 'text-warn-text' : 'text-muted')}>{n}</span>
      <span>{children}</span>
    </div>
  )
}

// --- menus -----------------------------------------------------------------------

export interface MenuItem {
  label: string
  icon?: ReactNode
  danger?: boolean
  /** a second line: what it does, or why it is off */
  hint?: string
  disabled?: boolean
  /** a choice in a list of choices */
  checked?: boolean
  /** a line above this item */
  divide?: boolean
  onSelect: () => void
}

export function Menu({
  label,
  children,
  items,
  className,
  variant = 'secondary',
  size = 'icon',
}: {
  /** the trigger's accessible name */
  label: string
  children: ReactNode
  items: MenuItem[]
  className?: string
  variant?: Variant
  size?: keyof typeof SIZES
}) {
  const [pos, setPos] = useState<{ top: number; left: number; up: boolean; anchorTop: number; anchorLeft: number } | null>(null)
  const anchor = useRef<HTMLButtonElement>(null)
  const panel = useRef<HTMLDivElement>(null)
  const open = pos !== null
  const radio = items.some((i) => i.checked !== undefined)
  const close = useCallback((refocus = false) => {
    setPos(null)
    if (refocus) anchor.current?.focus()
  }, [])

  // The menu is portalled to <body> with fixed coordinates, so no card or
  // table with overflow hidden can clip it.
  const toggle = () => {
    if (open) return close()
    const r = anchor.current!.getBoundingClientRect()
    const height = items.reduce((h, i) => h + (i.hint ? 56 : 40) + (i.divide ? 9 : 0), 10)
    const up = r.bottom + height + 8 > window.innerHeight && r.top > height + 8
    const width = 248
    setPos({
      top: up ? r.top - 4 : r.bottom + 4,
      left: Math.max(8, Math.min(r.right - width, window.innerWidth - width - 8)),
      up,
      anchorTop: r.top,
      anchorLeft: r.left,
    })
  }

  useLayoutEffect(() => {
    if (!pos) return
    // Scroll events arrive a frame late, so one from just before the click can
    // land after the menu opened. Close only if the trigger really moved.
    const onScroll = () => {
      const r = anchor.current?.getBoundingClientRect()
      if (!r || Math.abs(r.top - pos.anchorTop) > 2 || Math.abs(r.left - pos.anchorLeft) > 2) close()
    }
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node
      if (!anchor.current?.contains(t) && !panel.current?.contains(t)) close()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(true)
      else if (e.key === 'Tab') close()
    }
    const onResize = () => close()
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    window.addEventListener('scroll', onScroll, true)
    window.addEventListener('resize', onResize)
    const enabled = panel.current?.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([aria-disabled="true"])')
    ;(panel.current?.querySelector<HTMLElement>('[aria-checked="true"]') ?? enabled?.[0])?.focus({ preventScroll: true })
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('scroll', onScroll, true)
      window.removeEventListener('resize', onResize)
    }
  }, [pos, close])

  const onMenuKey = (e: ReactKeyboardEvent) => {
    const all = [...panel.current!.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([aria-disabled="true"])')]
    const i = all.indexOf(document.activeElement as HTMLElement)
    const next =
      e.key === 'ArrowDown' ? (i + 1) % all.length
      : e.key === 'ArrowUp' ? (i - 1 + all.length) % all.length
      : e.key === 'Home' ? 0
      : e.key === 'End' ? all.length - 1
      : -1
    if (next < 0) return
    e.preventDefault()
    all[next]?.focus()
  }

  return (
    <>
      <button
        ref={anchor}
        type="button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown' && !open) {
            e.preventDefault()
            toggle()
          }
        }}
        className={btn(variant, size, className)}
      >
        {children}
      </button>
      {pos &&
        createPortal(
          <div
            ref={panel}
            role="menu"
            aria-label={label}
            onKeyDown={onMenuKey}
            style={{ position: 'fixed', left: pos.left, top: pos.top, transform: pos.up ? 'translateY(-100%)' : undefined }}
            className="z-50 w-[248px] overflow-hidden rounded-md border border-border bg-surface p-1 shadow-lg"
          >
            {items.map((item) => (
              <div key={item.label}>
                {item.divide && <div className="mx-2 my-1 border-t border-border" />}
                <button
                  role={radio ? 'menuitemradio' : 'menuitem'}
                  aria-checked={radio ? !!item.checked : undefined}
                  aria-disabled={item.disabled || undefined}
                  tabIndex={-1}
                  type="button"
                  onClick={() => {
                    if (item.disabled) return
                    close(true)
                    item.onSelect()
                  }}
                  className={cx(
                    'flex min-h-[40px] w-full items-start gap-2.5 rounded-[6px] px-2.5 py-2 text-left text-[13px] focus:bg-track focus:outline-none',
                    item.disabled ? 'cursor-not-allowed text-faint' : 'hover:bg-track',
                    !item.disabled && (item.danger ? 'text-bad-text' : 'text-foreground'),
                  )}
                >
                  <span className="mt-0.5 shrink-0 text-muted [&>svg]:h-4 [&>svg]:w-4">
                    {radio ? item.checked ? <Check /> : <span className="block h-4 w-4" /> : item.icon}
                  </span>
                  <span className="flex min-w-0 flex-col">
                    {item.label}
                    {item.hint && <span className="text-xs text-faint">{item.hint}</span>}
                  </span>
                </button>
              </div>
            ))}
          </div>,
          document.body,
        )}
    </>
  )
}

/** A trigger's caption with the arrow that says it opens a list. */
export const Caret = () => <ChevronDown className="h-3.5 w-3.5 text-muted" />

// --- toasts ------------------------------------------------------------------

interface Toast {
  id: number
  text: string
  tone: 'ok' | 'error'
  action?: { label: string; run: () => void }
}

type Push = (text: string, tone?: Toast['tone'], action?: Toast['action']) => void

const ToastContext = createContext<Push>(() => {})

/** Toasts sit top right under the header on desktops, above the bottom edge on phones; errors stay until closed. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const drop = useCallback((id: number) => setToasts((t) => t.filter((x) => x.id !== id)), [])
  const push = useCallback<Push>(
    (text, tone = 'ok', action) => {
      const id = Date.now() + Math.random()
      setToasts((t) => [...t.slice(-3), { id, text, tone, action }])
      if (tone === 'ok') setTimeout(() => drop(id), 3500)
    },
    [drop],
  )
  return (
    <ToastContext.Provider value={push}>
      {children}
      <div data-toasts className="pointer-events-none fixed inset-x-4 bottom-[calc(16px+env(safe-area-inset-bottom)+var(--toast-lift,0px))] z-50 flex flex-col gap-2 sm:bottom-auto sm:left-auto sm:right-4 sm:top-[68px] sm:w-[380px]">
        {toasts.map((t) => (
          <div
            key={t.id}
            data-toast
            role={t.tone === 'error' ? 'alert' : 'status'}
            className={cx(
              'pointer-events-auto flex items-start gap-3 rounded-md border bg-surface px-4 py-3 text-[13px] shadow-lg',
              t.tone === 'error' ? 'border-bad text-foreground' : 'border-border-strong text-foreground',
            )}
          >
            {t.tone === 'error' && <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-bad-text" />}
            <span className="min-w-0 flex-1 break-words">{t.text}</span>
            {t.action && (
              <button
                type="button"
                className="-my-1 shrink-0 rounded px-1.5 py-1 font-medium underline underline-offset-2"
                onClick={() => {
                  drop(t.id)
                  t.action!.run()
                }}
              >
                {t.action.label}
              </button>
            )}
            {t.tone === 'error' && (
              <button type="button" aria-label="Dismiss" onClick={() => drop(t.id)} className="-m-1 shrink-0 rounded p-1 text-muted hover:text-foreground">
                <X className="h-4 w-4" />
              </button>
            )}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export const useToast = () => useContext(ToastContext)

export async function copyText(text: string) {
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
}

export function CopyButton({
  text,
  label = 'Copy',
  name,
  variant,
  size = 'sm',
  className,
}: {
  text: string
  label?: string
  /** a fuller name for screen readers, when the label alone is ambiguous */
  name?: string
  variant?: Variant
  size?: keyof typeof SIZES
  className?: string
}) {
  const [copied, setCopied] = useState(false)
  return (
    <Button
      size={size}
      variant={variant}
      className={className}
      aria-label={name}
      onClick={async () => {
        await copyText(text)
        setCopied(true)
        setTimeout(() => setCopied(false), 1500)
      }}
    >
      {copied ? <Check className="h-4 w-4 text-ok-text" /> : <Copy className="h-4 w-4" />}
      {copied ? 'Copied' : label}
      <span className="sr-only" aria-live="polite">
        {copied ? 'Copied' : ''}
      </span>
    </Button>
  )
}

export function QrCode({ text, size = 200 }: { text: string; size?: number }) {
  const svg = useMemo(() => {
    const qr = qrcode(0, 'M')
    qr.addData(text)
    qr.make()
    // The wrapper is the labelled image; hide the inner <svg> from screen readers.
    return qr.createSvgTag({ cellSize: 4, margin: 4, scalable: true }).replace('<svg ', '<svg aria-hidden="true" ')
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

const NOTICE = {
  info: 'border-border-strong bg-card2 [&>svg]:text-muted',
  warn: 'border-warn bg-warn-tint [&>svg]:text-warn-text',
  danger: 'border-bad bg-bad-tint [&>svg]:text-bad-text',
}

export function Notice({ tone = 'info', children, className }: { tone?: keyof typeof NOTICE; children: ReactNode; className?: string }) {
  const Icon = tone === 'info' ? Info : TriangleAlert
  return (
    <div className={cx('flex gap-3 rounded-md border p-3 text-[13px]', NOTICE[tone], className)}>
      <Icon className="mt-0.5 h-4 w-4 shrink-0" />
      <div className="min-w-0 break-words">{children}</div>
    </div>
  )
}

export function ErrorBox({ children }: { children: ReactNode }) {
  return (
    <div role="alert" className="whitespace-pre-wrap break-words rounded-md border border-bad bg-bad-tint px-3 py-2 text-[13px] text-bad-text">
      {children}
    </div>
  )
}

// --- choices -------------------------------------------------------------------

/** A filter chip that stays pressed while it is the one in use. */
export function Chip({ on, count, children, onClick }: { on: boolean; count?: number; children: ReactNode; onClick: () => void }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      onClick={onClick}
      className={cx(
        'inline-flex h-11 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full border px-3.5 text-[13px] transition sm:h-[30px] sm:px-3',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent',
        on ? 'border-accent bg-accent text-accent-fg' : 'border-border bg-card text-foreground hover:border-border-strong',
      )}
    >
      {children}
      {count !== undefined && <span className="tabular opacity-70">{count}</span>}
    </button>
  )
}

/** One of a few options, side by side. */
export function Segmented<T extends string | number>({
  label,
  value,
  options,
  onChange,
  className,
}: {
  label: string
  value: T
  options: [T, string][]
  onChange: (v: T) => void
  className?: string
}) {
  return (
    <div role="group" aria-label={label} className={cx('flex rounded-md border border-border bg-card2 p-0.5', className)}>
      {options.map(([v, text]) => (
        <button
          key={String(v)}
          type="button"
          aria-pressed={v === value}
          onClick={() => onChange(v)}
          className={cx(
            'h-10 flex-1 whitespace-nowrap rounded-[6px] px-3 text-[13px] font-medium transition sm:h-7',
            'focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent',
            v === value ? 'bg-accent text-accent-fg' : 'text-muted hover:text-foreground',
          )}
        >
          {text}
        </button>
      ))}
    </div>
  )
}

/** A section that opens and closes, closed at first. */
export function Disclosure({
  title,
  hint,
  children,
  className,
  defaultOpen = false,
}: {
  title: string
  hint?: string
  children: ReactNode
  className?: string
  defaultOpen?: boolean
}) {
  const [open, setOpen] = useState(defaultOpen)
  const id = useId()
  return (
    <div className={className}>
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => setOpen(!open)}
        className="flex min-h-[44px] w-full items-center justify-between gap-3 text-left focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent sm:min-h-0"
      >
        <span className="flex flex-col">
          <span className="font-semibold">{title}</span>
          {hint && <span className="text-[13px] text-muted">{hint}</span>}
        </span>
        <ChevronDown className={cx('h-4 w-4 shrink-0 text-muted transition', open && 'rotate-180')} />
      </button>
      {open && (
        <div id={id} className="mt-3">
          {children}
        </div>
      )}
    </div>
  )
}

/** Tabs with arrow keys that move between them, as screen readers expect. */
export function Tabs<T extends string>({
  label,
  tabs,
  value,
  onChange,
  idBase,
}: {
  label: string
  tabs: [T, string][]
  value: T
  onChange: (t: T) => void
  idBase: string
}) {
  const list = useRef<HTMLDivElement>(null)
  const onKey = (e: ReactKeyboardEvent) => {
    // from the tab that has focus: a fast second key can beat the re-render
    const all = [...list.current!.querySelectorAll<HTMLElement>('[role="tab"]')]
    const at = all.indexOf(document.activeElement as HTMLElement)
    const i = at >= 0 ? at : tabs.findIndex(([t]) => t === value)
    const next =
      e.key === 'ArrowRight' ? (i + 1) % tabs.length
      : e.key === 'ArrowLeft' ? (i - 1 + tabs.length) % tabs.length
      : e.key === 'Home' ? 0
      : e.key === 'End' ? tabs.length - 1
      : -1
    if (next < 0) return
    e.preventDefault()
    onChange(tabs[next][0])
    all[next]?.focus()
  }
  return (
    <div ref={list} role="tablist" aria-label={label} onKeyDown={onKey} className="no-scrollbar flex gap-1 overflow-x-auto border-b border-border">
      {tabs.map(([t, text]) => (
        <button
          key={t}
          id={`${idBase}-${t}`}
          type="button"
          role="tab"
          aria-selected={t === value}
          aria-controls={`${idBase}-panel`}
          tabIndex={t === value ? 0 : -1}
          onClick={() => onChange(t)}
          className={cx(
            '-mb-px h-11 shrink-0 border-b-2 px-3 text-[13px] font-medium sm:h-auto sm:py-2',
            'focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent',
            t === value ? 'border-accent text-foreground' : 'border-transparent text-muted hover:text-foreground',
          )}
        >
          {text}
        </button>
      ))}
    </div>
  )
}
