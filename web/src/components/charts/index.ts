/**
 * The chart kit, in one import.
 *
 * Pages compose from here and never import Recharts: the wrapping is what keeps
 * colour, axes, tooltips, empty states and the written summary consistent across
 * a dozen views, and what makes a change to any of those one edit rather than
 * twelve.
 */

export { BarChart } from './BarChart'
export type { BarChartProps, BarDatum } from './BarChart'

export { ChartCard } from './ChartCard'
export type { ChartCardProps } from './ChartCard'

export { ChartEmpty, ChartFrame, TooltipCard } from './ChartFrame'
export type { ChartA11yProps, ChartEmptyProps, ChartFrameProps, TooltipRow } from './ChartFrame'

export { Heatmap } from './Heatmap'
export type { HeatmapProps } from './Heatmap'

export { HourChart } from './HourChart'
export type { HourChartProps } from './HourChart'

export { ShareBar } from './ShareBar'
export type { ShareBarProps } from './ShareBar'

export { Sparkline } from './Sparkline'
export type { SparklineProps } from './Sparkline'

export { MetricToggle, TimelineChart } from './TimelineChart'
export type { MetricToggleProps, TimelineChartProps, TimelineMetric } from './TimelineChart'

export { WeekdayChart } from './WeekdayChart'
export type { WeekdayChartProps } from './WeekdayChart'

export {
  HEAT_STEPS,
  SERIES_LIMIT,
  heatColor,
  readChartPalette,
  seriesColor,
  useChartPalette,
} from './palette'
export type { ChartPalette } from './palette'
