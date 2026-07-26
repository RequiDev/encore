/**
 * The 24-hour clock.
 *
 * Every third hour is labelled; the rest are still columns, still hoverable and
 * still named in the tooltip and the summary. Labelling all twenty-four would
 * collide on a phone, and a tick every three hours is enough to read a shape by.
 */

import type { ReactElement, ReactNode } from 'react'
import { useCallback } from 'react'
import { formatHour } from '../../lib/format'
import type { RepartitionBucket } from '../../lib/types'
import { RepartitionColumns } from './RepartitionColumns'
import type { TimelineMetric } from './TimelineChart'

export interface HourChartProps {
  /** 24 buckets, local hour of day. */
  buckets: RepartitionBucket[]
  metric?: TimelineMetric
  slot?: number
  height?: number
  busy?: boolean
  emptyAction?: ReactNode
}

export function HourChart({
  buckets,
  metric = 'plays',
  slot = 0,
  height = 220,
  busy = false,
  emptyAction,
}: HourChartProps): ReactElement {
  // Only the three-hourly ticks carry text; `formatHour` gives `08:00`, of which
  // the first two characters are the hour.
  const axisLabel = useCallback(
    (key: number) => (key % 3 === 0 ? formatHour(key).slice(0, 2) : ''),
    [],
  )
  const fullLabel = useCallback((key: number) => formatHour(key), [])

  return (
    <RepartitionColumns
      buckets={buckets}
      metric={metric}
      axisLabel={axisLabel}
      fullLabel={fullLabel}
      label={metric === 'plays' ? 'Listens by hour of day' : 'Listening time by hour of day'}
      noun="hour"
      slot={slot}
      height={height}
      busy={busy}
      emptyAction={emptyAction}
    />
  )
}
