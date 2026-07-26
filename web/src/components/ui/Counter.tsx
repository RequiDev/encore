/**
 * The counter: Encore's signature figure.
 *
 * Monospaced with tabular figures, so a column of them aligns digit for digit
 * and a number that ticks up does not shuffle the ones beside it. The optional
 * meter underneath is a hairline share bar — how much of the whole this figure
 * is — which is the only decoration a counter ever gets.
 */

import type { ReactElement, ReactNode } from 'react'

export interface CounterProps {
  /** Pre-formatted. Numbers reach here through `lib/format`, never raw. */
  value: ReactNode
  /** A unit or qualifier set beside the figure at ordinary size. */
  suffix?: ReactNode
  /** Lights the figure amber. Reserve it for the one figure a panel is about. */
  lamp?: boolean
  /** Share of the whole, 0-1, drawn as the hairline meter beneath the figure. */
  meter?: number
  /** What the meter is a share of, announced to assistive technology. */
  meterLabel?: string
  className?: string
}

export function Counter({
  value,
  suffix,
  lamp = false,
  meter,
  meterLabel,
  className,
}: CounterProps): ReactElement {
  const share =
    typeof meter === 'number' && Number.isFinite(meter) ? Math.min(Math.max(meter, 0), 1) : null

  return (
    <div className={['min-w-0', className].filter(Boolean).join(' ')}>
      <div className="flex items-baseline gap-1.5">
        <span className={lamp ? 'counter counter-lamp' : 'counter'}>{value}</span>
        {suffix ? <span className="text-sm text-ink-muted">{suffix}</span> : null}
      </div>
      {share !== null ? (
        <div
          className="meter mt-2"
          role="meter"
          aria-valuenow={Math.round(share * 100)}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={meterLabel ?? 'Share of total'}
        >
          <span style={{ width: `${share * 100}%` }} />
        </div>
      ) : null}
    </div>
  )
}
