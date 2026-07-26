/**
 * A chip: a small state marker.
 *
 * Used for import job status, listen source, sync state — anywhere a word needs
 * to read as a label rather than as prose. Tone carries meaning, but never on
 * its own: the word inside always says the same thing the colour does, because
 * a chip that only means something to people who can distinguish amber from
 * teal is not a status indicator.
 */

import type { ReactElement, ReactNode } from 'react'

export type ChipTone = 'neutral' | 'lamp' | 'good' | 'warn' | 'bad' | 'info'

const TONES: Record<ChipTone, string> = {
  neutral: '',
  lamp: 'border-lamp text-lamp',
  good: 'border-sage text-sage',
  warn: 'border-lamp-dim text-lamp-dim',
  bad: 'border-ember text-ember',
  info: 'border-signal text-signal',
}

export interface ChipProps {
  tone?: ChipTone
  className?: string
  children: ReactNode
}

export function Chip({ tone = 'neutral', className, children }: ChipProps): ReactElement {
  return (
    <span className={['chip', TONES[tone], className].filter(Boolean).join(' ')}>{children}</span>
  )
}
