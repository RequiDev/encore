/**
 * The week as a grid: seven days down, twenty-four hours across.
 *
 * This one is not drawn with Recharts. A heatmap is a table — rows, columns and
 * a value at each crossing — and building it as a real `<table>` means a screen
 * reader gets the whole thing for free: "Saturday, 22:00, 132 plays", read out
 * of the row and column headers, with no parallel description to keep in step.
 * Sighted readers get the same values from the shading and the readout.
 *
 * The scale is one hue, pale to deep, derived from the lamp — never a rainbow.
 * A cell with nothing in it is not the palest step: it is the empty colour, so
 * "nothing happened" cannot be mistaken for "a little happened".
 */

import type { KeyboardEvent, ReactElement, ReactNode } from 'react'
import { useCallback, useMemo, useRef, useState } from 'react'
import {
  formatCount,
  formatDuration,
  formatHour,
  formatPlural,
  formatRatio,
  formatWeekday,
} from '../../lib/format'
import type { HeatmapCell } from '../../lib/types'
import { ChartEmpty } from './ChartFrame'
import type { TimelineMetric } from './TimelineChart'
import { HEAT_STEPS, heatColor, useChartPalette } from './palette'

const DAYS = 7
const HOURS = 24

const FULL_DAYS = ['Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday', 'Sunday']

interface Slot {
  weekday: number
  hour: number
  plays: number
  msPlayed: number
}

function dayName(weekday: number): string {
  return FULL_DAYS[((weekday % DAYS) + DAYS) % DAYS] ?? formatWeekday(weekday)
}

function valueOf(slot: Slot, metric: TimelineMetric): number {
  return metric === 'plays' ? slot.plays : slot.msPlayed
}

function formatValue(value: number, metric: TimelineMetric): string {
  return metric === 'plays' ? formatPlural(value, 'play') : formatDuration(value)
}

export interface HeatmapProps {
  /** The 7 × 24 grid. Missing crossings are treated as nothing played. */
  cells: HeatmapCell[]
  metric?: TimelineMetric
  busy?: boolean
  emptyAction?: ReactNode
}

export function Heatmap({
  cells,
  metric = 'plays',
  busy = false,
  emptyAction,
}: HeatmapProps): ReactElement {
  const palette = useChartPalette()
  const grid = useRef<HTMLTableElement>(null)
  // The cell Tab lands on. It moves with the arrow keys so the grid is one stop
  // in the tab order rather than a hundred and sixty-eight.
  const [cursor, setCursor] = useState({ weekday: 0, hour: 0 })
  const [hovered, setHovered] = useState<Slot | null>(null)
  const [focused, setFocused] = useState<Slot | null>(null)

  const slots = useMemo(() => {
    const table: Slot[][] = Array.from({ length: DAYS }, (_, weekday) =>
      Array.from({ length: HOURS }, (_, hour) => ({ weekday, hour, plays: 0, msPlayed: 0 })),
    )
    for (const cell of cells ?? []) {
      const row = table[cell.weekday]
      const slot = row?.[cell.hour]
      if (!slot) continue
      slot.plays = cell.plays
      slot.msPlayed = cell.msPlayed
    }
    return table
  }, [cells])

  const flat = useMemo(() => slots.flat(), [slots])
  const max = useMemo(
    () => flat.reduce((best, slot) => Math.max(best, valueOf(slot, metric)), 0),
    [flat, metric],
  )
  const total = useMemo(
    () => flat.reduce((sum, slot) => sum + valueOf(slot, metric), 0),
    [flat, metric],
  )
  const busiest = useMemo(
    () =>
      flat.reduce<Slot | null>(
        (best, slot) => (!best || valueOf(slot, metric) > valueOf(best, metric) ? slot : best),
        null,
      ),
    [flat, metric],
  )

  const move = useCallback((weekday: number, hour: number) => {
    const target = grid.current?.querySelector<HTMLButtonElement>(
      `[data-cell="${weekday}-${hour}"]`,
    )
    target?.focus()
  }, [])

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>, weekday: number, hour: number) => {
      const keys: Record<string, [number, number]> = {
        ArrowRight: [weekday, hour + 1],
        ArrowLeft: [weekday, hour - 1],
        ArrowDown: [weekday + 1, hour],
        ArrowUp: [weekday - 1, hour],
        Home: [weekday, 0],
        End: [weekday, HOURS - 1],
      }
      const next = keys[event.key]
      if (!next) return
      event.preventDefault()
      const [w, h] = next
      move(Math.min(Math.max(w, 0), DAYS - 1), Math.min(Math.max(h, 0), HOURS - 1))
    },
    [move],
  )

  if (total <= 0) {
    return (
      <ChartEmpty
        height={220}
        description="Nothing was played in this range, so there is no weekly pattern yet."
        action={emptyAction}
      />
    )
  }

  const active = hovered ?? focused
  const caption =
    `Listening by day of the week and hour of the day, as a seven by twenty-four grid. ` +
    (busiest
      ? `Busiest: ${dayName(busiest.weekday)} at ${formatHour(busiest.hour)}, ` +
        `${formatValue(valueOf(busiest, metric), metric)} — ` +
        `${formatRatio(valueOf(busiest, metric), total)} of the range. `
      : '') +
    `Each cell gives its own figure.`

  return (
    <div aria-busy={busy || undefined} className={busy ? 'opacity-60' : undefined}>
      <div className="w-full overflow-x-auto">
        <table ref={grid} className="w-full min-w-[480px] table-fixed border-collapse">
          <caption className="sr-only">{caption}</caption>
          <thead>
            <tr>
              <td className="w-10" />
              {Array.from({ length: HOURS }, (_, hour) => (
                <th key={hour} scope="col" className="p-0 pb-1 align-bottom">
                  <span aria-hidden="true" className="tabular block text-[10px] text-ink-faint">
                    {hour % 3 === 0 ? formatHour(hour).slice(0, 2) : ' '}
                  </span>
                  <span className="sr-only">{formatHour(hour)}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody
            onBlur={(event) => {
              if (!event.currentTarget.contains(event.relatedTarget)) setFocused(null)
            }}
            onMouseLeave={() => setHovered(null)}
          >
            {slots.map((row, weekday) => (
              <tr key={weekday}>
                <th scope="row" className="pr-2 text-right text-[11px] font-normal text-ink-muted">
                  <span aria-hidden="true">{formatWeekday(weekday)}</span>
                  <span className="sr-only">{dayName(weekday)}</span>
                </th>
                {row.map((slot) => {
                  const value = valueOf(slot, metric)
                  return (
                    <td key={slot.hour} className="p-px">
                      <button
                        type="button"
                        data-cell={`${slot.weekday}-${slot.hour}`}
                        tabIndex={
                          cursor.weekday === slot.weekday && cursor.hour === slot.hour ? 0 : -1
                        }
                        aria-label={`${formatValue(value, metric)}${
                          metric === 'plays' && slot.msPlayed > 0
                            ? `, ${formatDuration(slot.msPlayed)}`
                            : ''
                        }`}
                        onFocus={() => {
                          setCursor({ weekday: slot.weekday, hour: slot.hour })
                          setFocused(slot)
                        }}
                        onMouseEnter={() => setHovered(slot)}
                        onKeyDown={(event) => onKeyDown(event, slot.weekday, slot.hour)}
                        className="block h-5 w-full rounded-[2px] transition-opacity hover:opacity-80"
                        style={{ backgroundColor: heatColor(palette, value, max) }}
                      />
                    </td>
                  )
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="mt-3 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <p aria-hidden="true" className="min-h-4 text-xs text-ink-muted">
          {active ? (
            <>
              <span className="text-ink">
                {dayName(active.weekday)} {formatHour(active.hour)}
              </span>
              <span className="tabular ml-2 text-ink">
                {metric === 'plays' ? formatCount(active.plays) : formatDuration(active.msPlayed)}
              </span>
              <span className="ml-1">{metric === 'plays' ? 'plays' : ''}</span>
            </>
          ) : (
            'Point at a cell, or use the arrow keys, for its figure.'
          )}
        </p>
        <ScaleLegend max={max} metric={metric} />
      </div>
    </div>
  )
}

/** The key to the ramp: what pale means and what deep means. */
function ScaleLegend({ max, metric }: { max: number; metric: TimelineMetric }): ReactElement {
  const palette = useChartPalette()
  return (
    <p className="flex items-center gap-1.5 text-xs text-ink-faint">
      <span className="eyebrow">None</span>
      <span
        aria-hidden="true"
        className="h-3 w-4 rounded-[2px]"
        style={{ backgroundColor: palette.empty }}
      />
      {Array.from({ length: HEAT_STEPS }, (_, step) => (
        <span
          key={step}
          aria-hidden="true"
          className="h-3 w-4 rounded-[2px]"
          style={{ backgroundColor: palette.heat[step] ?? palette.lamp }}
        />
      ))}
      <span className="eyebrow">
        {metric === 'plays' ? formatPlural(max, 'play') : formatDuration(max)}
      </span>
    </p>
  )
}
