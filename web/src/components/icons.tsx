// Icons, drawn the way nasnet-panel draws them: lucide at stroke 1.75. Import
// icons from here, not from lucide-react, so the style stays consistent.
import type { LucideIcon, LucideProps } from 'lucide-react'
import {
  Activity as LActivity,
  Check as LCheck,
  ChevronLeft as LChevronLeft,
  ChevronRight as LChevronRight,
  CircleCheck as LCircleCheck,
  Circle as LCircle,
  CircleX as LCircleX,
  Copy as LCopy,
  Download as LDownload,
  Ellipsis as LEllipsis,
  KeyRound as LKeyRound,
  Loader2 as LLoader2,
  Moon as LMoon,
  Pencil as LPencil,
  Plus as LPlus,
  Power as LPower,
  PowerOff as LPowerOff,
  QrCode as LQrCode,
  RefreshCw as LRefreshCw,
  RotateCcw as LRotateCcw,
  ScrollText as LScrollText,
  Search as LSearch,
  SearchX as LSearchX,
  SlidersHorizontal as LSlidersHorizontal,
  Sun as LSun,
  Trash2 as LTrash2,
  TriangleAlert as LTriangleAlert,
  Users as LUsers,
  X as LX,
} from 'lucide-react'

const panel = (Icon: LucideIcon) => {
  const Styled = (props: LucideProps) => <Icon strokeWidth={1.75} aria-hidden {...props} />
  Styled.displayName = Icon.displayName
  return Styled
}

export const Activity = panel(LActivity)
export const Check = panel(LCheck)
export const ChevronLeft = panel(LChevronLeft)
export const ChevronRight = panel(LChevronRight)
export const CircleCheck = panel(LCircleCheck)
export const Circle = panel(LCircle)
export const CircleX = panel(LCircleX)
export const Copy = panel(LCopy)
export const Download = panel(LDownload)
export const Ellipsis = panel(LEllipsis)
export const KeyRound = panel(LKeyRound)
export const Loader = panel(LLoader2)
export const Moon = panel(LMoon)
export const Pencil = panel(LPencil)
export const Plus = panel(LPlus)
export const Power = panel(LPower)
export const PowerOff = panel(LPowerOff)
export const QrCode = panel(LQrCode)
export const RefreshCw = panel(LRefreshCw)
export const RotateCcw = panel(LRotateCcw)
export const ScrollText = panel(LScrollText)
export const Search = panel(LSearch)
export const SearchX = panel(LSearchX)
export const SlidersHorizontal = panel(LSlidersHorizontal)
export const Sun = panel(LSun)
export const Trash2 = panel(LTrash2)
export const TriangleAlert = panel(LTriangleAlert)
export const Users = panel(LUsers)
export const X = panel(LX)

/** nasnet-panel's Telegram MTProto plugin logo (routes/plugins/PluginLogos.tsx). */
export function TelegramMtprotoLogo({ size = 32 }: { size?: number }) {
  return (
    <svg viewBox="0 0 64 64" width={size} height={size} role="img" aria-label="Telegram MTProto logo">
      <defs>
        <linearGradient id="telegram-grad" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#37BBFE" />
          <stop offset="100%" stopColor="#007DBB" />
        </linearGradient>
      </defs>
      <rect width="64" height="64" rx="14" fill="url(#telegram-grad)" />
      <path
        d="M46.5 19.2 41 46.4c-.4 1.8-1.5 2.3-3 1.4l-8.3-6.1-4 3.8c-.4.4-.8.8-1.7.8l.6-8.6 15.6-14.1c.7-.6-.2-1-1-.4L20 33l-8.4-2.6c-1.8-.6-1.9-1.8.4-2.7l32.7-12.6c1.5-.5 2.9.4 1.8 4.1Z"
        fill="#ffffff"
      />
    </svg>
  )
}
