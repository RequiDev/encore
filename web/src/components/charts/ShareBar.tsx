/**
 * One proportion, drawn once.
 *
 * "This artist is 12% of your listening" is a single ratio against a whole, so
 * it is a meter and not a chart — a two-slice pie or a one-bar bar chart would
 * be a picture of one number. The track is the same hue as the fill, a step
 * lighter, so the state reads across the whole bar rather than only the filled
 * part.
 *
 * The percentage is always written out beside it. Nobody should have to measure
 * a bar with their eye to read a value the interface already knows.
 */

import type { ReactElement, ReactNode } from 'react'
import { formatPercent } from '../../lib/format'
import { seriesColor, useChartPalette } from './palette'

export interface ShareBarProps {
  /** The part. */
  value: number
  /** The whole. A zero or negative whole reads as no share, never as an error. */
  total: number
  /** What the share is of: "of your listening time". */
  label: ReactNode
  /** Both figures spelled out, e.g. "4h 12m of 1d 6h". */
  detail?: ReactNode
  slot?: number
  className?: string
}

export function ShareBar({
  value,
  total,
  label,
  detail,
  slot = 0,
  className,
}: ShareBarProps): ReactElement {
  const palette = useChartPalette()
  const colour = seriesColor(palette, slot)

  const usable = Number.isFinite(value) && Number.isFinite(total) && total > 0 ? value / total : 0
  const share = Math.min(Math.max(usable, 0), 1)
  const percent = formatPercent(share)

  return (
    <div className={['min-w-0', className].filter(Boolean).join(' ')}>
      <div className="flex items-baseline justify-between gap-3">
        <p className="min-w-0 truncate text-xs text-ink-muted">{label}</p>
        <p className="tabular shrink-0 text-sm font-semibold text-ink">{percent}</p>
      </div>
      <div
        role="meter"
        aria-valuenow={Math.round(share * 100)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuetext={percent}
        className="mt-1.5 h-2 w-full overflow-hidden rounded-[2px]"
        style={{ backgroundColor: palette.empty }}
      >
        <span
          className="block h-full rounded-[2px]"
          style={{ width: `${share * 100}%`, backgroundColor: colour }}
        />
      </div>
      {detail ? <p className="mt-1.5 text-xs text-ink-faint">{detail}</p> : null}
    </div>
  )
}
