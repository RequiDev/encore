/**
 * The seven days of the week, Monday first — the order the API buckets in, and
 * the order a listening week actually reads in.
 */

import type { ReactElement, ReactNode } from 'react'
import { useCallback } from 'react'
import { formatWeekday } from '../../lib/format'
import type { RepartitionBucket } from '../../lib/types'
import { RepartitionColumns } from './RepartitionColumns'
import type { TimelineMetric } from './TimelineChart'

const FULL_DAYS = ['Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday', 'Sunday']

export interface WeekdayChartProps {
  /** 7 buckets, 0 = Monday. */
  buckets: RepartitionBucket[]
  metric?: TimelineMetric
  slot?: number
  height?: number
  busy?: boolean
  emptyAction?: ReactNode
}

export function WeekdayChart({
  buckets,
  metric = 'plays',
  slot = 0,
  height = 220,
  busy = false,
  emptyAction,
}: WeekdayChartProps): ReactElement {
  const axisLabel = useCallback((key: number) => formatWeekday(key), [])
  // The tooltip and the summary have room for the whole word; the axis does not.
  const fullLabel = useCallback(
    (key: number) => FULL_DAYS[((key % 7) + 7) % 7] ?? formatWeekday(key),
    [],
  )

  return (
    <RepartitionColumns
      buckets={buckets}
      metric={metric}
      axisLabel={axisLabel}
      fullLabel={fullLabel}
      label={
        metric === 'plays' ? 'Listens by day of the week' : 'Listening time by day of the week'
      }
      noun="day"
      slot={slot}
      height={height}
      busy={busy}
      emptyAction={emptyAction}
    />
  )
}
