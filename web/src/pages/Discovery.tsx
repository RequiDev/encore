/**
 * Discovery: when you heard something for the first time.
 *
 * The distinction this page rests on is that "new" means new *ever*, not new in
 * the range being looked at. An artist counts once, in the bucket holding their
 * very first play in your whole history, and never again — which is why a month
 * of heavy listening to old favourites can show almost no discovery at all. The
 * server derives the firsts from the entire history and only then buckets the
 * ones that fall inside the range, so the figure is stable no matter what dates
 * are on screen. The sentence saying so is on the page, not in this comment,
 * because a statistic nobody can interpret is worse than no statistic.
 *
 * The two charts are ranked rather than chronological: the shape over time is
 * carried by the sparklines and by the table, and a ranked bar answers the
 * question people actually bring here — "when did I find the most?".
 */

import type { ReactElement } from 'react'
import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import {
  EMPTY,
  formatCount,
  formatDate,
  formatDateTime,
  formatDayKey,
  formatMonth,
  formatPlural,
} from '../lib/format'
import type { DiscoveryBucket, Interval } from '../lib/types'
import {
  Button,
  ButtonLink,
  EmptyState,
  ErrorState,
  Field,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Panel,
  RangePicker,
  Select,
  Skeleton,
  SkeletonLedger,
  Stat,
  StatGrid,
} from '../components/ui'
import { BarChart, ChartCard, Sparkline } from '../components/charts'
import type { BarDatum } from '../components/charts'

/** The server's own cap on how many points one response may carry. */
const MAX_BUCKETS = 1500

/** Above this the page picks a coarser default, so the table stays readable. */
const COMFORTABLE_BUCKETS = 120

/** How many periods the ranked charts show. */
const RANKED_ROWS = 12

interface IntervalOption {
  id: Interval
  label: string
  noun: string
  /** The nominal width the server estimates bucket counts with. */
  approxMs: number
}

/** The widest bucket, and the one every range is guaranteed to allow. */
const YEARLY: IntervalOption = {
  id: 'year',
  label: 'By year',
  noun: 'year',
  approxMs: 31_536_000_000,
}

const INTERVALS: readonly IntervalOption[] = [
  { id: 'hour', label: 'By hour', noun: 'hour', approxMs: 3_600_000 },
  { id: 'day', label: 'By day', noun: 'day', approxMs: 86_400_000 },
  { id: 'week', label: 'By week', noun: 'week', approxMs: 604_800_000 },
  { id: 'month', label: 'By month', noun: 'month', approxMs: 2_592_000_000 },
  YEARLY,
]

/** The same estimate the server makes, so the page never offers a 400. */
function bucketCount(durationMs: number, approxMs: number): number {
  return Math.floor(durationMs / approxMs) + 1
}

function labelFor(bucket: string, interval: Interval, timeZone: string): string {
  switch (interval) {
    case 'hour':
      return formatDateTime(bucket, timeZone)
    case 'week':
      return `Week of ${formatDate(bucket, timeZone)}`
    case 'month':
      return formatMonth(bucket, timeZone)
    case 'year':
      // The day key is `2026-07-26`; its first four characters are the year.
      return formatDayKey(bucket, timeZone).slice(0, 4)
    default:
      return formatDate(bucket, timeZone)
  }
}

export default function Discovery(): ReactElement {
  const { range, label, timeZone, setPreset } = useRange()
  const [chosen, setChosen] = useState<Interval | null>(null)

  const durationMs = Math.max(Date.parse(range.to) - Date.parse(range.from), 1)

  // Only the widths the server would accept are offered, and the default is the
  // finest one that still leaves a table a person can read.
  const allowed = useMemo(
    () => INTERVALS.filter((option) => bucketCount(durationMs, option.approxMs) <= MAX_BUCKETS),
    [durationMs],
  )
  const preferred =
    allowed.find((option) => bucketCount(durationMs, option.approxMs) <= COMFORTABLE_BUCKETS) ??
    allowed[0] ??
    YEARLY
  // Deriving rather than storing means a range change that outlaws the chosen
  // width corrects itself, with no effect and no flash of an invalid request.
  const active = allowed.find((option) => option.id === chosen) ?? preferred

  const discovery = useQuery({
    queryKey: qk.discovery(range, active.id),
    queryFn: ({ signal }) =>
      api.get<DiscoveryBucket[]>(
        '/stats/discovery',
        { from: range.from, to: range.to, interval: active.id },
        signal,
      ),
  })

  const buckets = useMemo(() => discovery.data ?? [], [discovery.data])
  const rows = useMemo(
    () =>
      buckets.map((bucket) => ({
        key: bucket.bucket,
        label: labelFor(bucket.bucket, active.id, timeZone),
        newArtists: bucket.newArtists,
        newTracks: bucket.newTracks,
      })),
    [buckets, active.id, timeZone],
  )

  const totalArtists = rows.reduce((sum, row) => sum + row.newArtists, 0)
  const totalTracks = rows.reduce((sum, row) => sum + row.newTracks, 0)
  const withDiscoveries = rows.filter((row) => row.newArtists > 0 || row.newTracks > 0)
  const busiest = rows.reduce<(typeof rows)[number] | null>(
    (best, row) => (best === null || row.newArtists > best.newArtists ? row : best),
    null,
  )

  const ranked = (pick: (row: (typeof rows)[number]) => number): BarDatum[] =>
    [...rows]
      .filter((row) => pick(row) > 0)
      .sort((a, b) => pick(b) - pick(a))
      .slice(0, RANKED_ROWS)
      .map((row) => ({ key: row.key, label: row.label, value: pick(row) }))

  const nothingNew = discovery.isSuccess && totalArtists === 0 && totalTracks === 0

  const status = discovery.isPending
    ? `Loading first-time listens for ${label.toLowerCase()}.`
    : discovery.isError
      ? 'Your discovery figures could not be loaded.'
      : nothingNew
        ? `Nothing was heard for the first time in ${label.toLowerCase()}.`
        : `${formatPlural(totalArtists, 'artist')} and ${formatPlural(totalTracks, 'track')} heard for the first time.`

  const explanation =
    'New means the first time ever, not the first time in this range: an artist counts once, in the period you first heard them, and never again.'

  return (
    <div className="space-y-4">
      <PageHeader
        title="Discovery"
        description={`What you heard for the first time — ${label.toLowerCase()}.`}
        actions={
          <>
            <Field label="Period width" labelHidden className="w-36">
              <Select
                value={active.id}
                onChange={(event) => setChosen(event.target.value as Interval)}
              >
                {allowed.map((option) => (
                  <option key={option.id} value={option.id}>
                    {option.label}
                  </option>
                ))}
              </Select>
            </Field>
            <RangePicker />
          </>
        }
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <p className="max-w-prose text-sm text-ink-muted">{explanation}</p>

      {discovery.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={discovery.error}
            title="Your discovery figures could not be loaded"
            onRetry={() => {
              void discovery.refetch()
            }}
          />
        </Panel>
      ) : nothingNew ? (
        <Panel padded={false}>
          <EmptyState
            icon="discovery"
            title="Nothing new in this range"
            description="Everything you played between these dates, you had heard before. Widen the range to find the periods where you were exploring."
            action={
              <div className="flex flex-wrap items-center justify-center gap-2">
                <Button variant="primary" onClick={() => setPreset('all')}>
                  Show all time
                </Button>
                <ButtonLink to="/imports">Import more history</ButtonLink>
              </div>
            }
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={3}>
            <Stat
              label="New artists"
              value={formatCount(totalArtists)}
              lamp
              loading={discovery.isPending}
              hint={
                <>
                  <span>First heard in this range</span>
                  <Sparkline
                    className="mt-1 block"
                    values={rows.map((row) => row.newArtists)}
                    label={`New artists per ${active.noun}`}
                    width={88}
                    height={18}
                  />
                </>
              }
            />
            <Stat
              label="New tracks"
              value={formatCount(totalTracks)}
              loading={discovery.isPending}
              hint={
                <>
                  <span>First heard in this range</span>
                  <Sparkline
                    className="mt-1 block"
                    values={rows.map((row) => row.newTracks)}
                    label={`New tracks per ${active.noun}`}
                    slot={1}
                    width={88}
                    height={18}
                  />
                </>
              }
            />
            <Stat
              label="Most new artists"
              value={busiest && busiest.newArtists > 0 ? busiest.label : EMPTY}
              loading={discovery.isPending}
              hint={
                discovery.isPending
                  ? 'Looking for it'
                  : busiest && busiest.newArtists > 0
                    ? `${formatPlural(busiest.newArtists, 'artist')} first heard that ${active.noun}`
                    : `No ${active.noun} in this range brought a new artist`
              }
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <ChartCard
              title="Where the artists came from"
              description={`The ${active.noun}s with the most first-time artists`}
            >
              {discovery.isPending ? (
                <ChartLoading label="Loading new artists per period" />
              ) : (
                <BarChart
                  data={ranked((row) => row.newArtists)}
                  label={`First-time artists by ${active.noun}, most first`}
                  valueName="new artists"
                  busy={discovery.isFetching}
                  emptyDescription="No artist was heard for the first time in this range."
                />
              )}
            </ChartCard>

            <ChartCard
              title="Where the tracks came from"
              description={`The ${active.noun}s with the most first-time tracks`}
            >
              {discovery.isPending ? (
                <ChartLoading label="Loading new tracks per period" />
              ) : (
                <BarChart
                  data={ranked((row) => row.newTracks)}
                  label={`First-time tracks by ${active.noun}, most first`}
                  valueName="new tracks"
                  slot={1}
                  busy={discovery.isFetching}
                  emptyDescription="No track was heard for the first time in this range."
                />
              )}
            </ChartCard>
          </div>

          <Panel
            title="Every period"
            description={`Oldest first. ${active.label.replace('By ', 'One row per ')}; periods with nothing new are left out.`}
            padded={false}
          >
            {discovery.isPending ? (
              <SkeletonLedger rows={8} columns={3} />
            ) : (
              <Ledger caption={`First-time artists and tracks per ${active.noun}, oldest first`}>
                <LedgerHead>
                  <LedgerRow>
                    <LedgerHeaderCell>Period</LedgerHeaderCell>
                    <LedgerHeaderCell numeric>New artists</LedgerHeaderCell>
                    <LedgerHeaderCell numeric>New tracks</LedgerHeaderCell>
                  </LedgerRow>
                </LedgerHead>
                <LedgerBody>
                  {withDiscoveries.map((row) => (
                    <LedgerRow key={row.key}>
                      <LedgerRowHeader className="whitespace-nowrap">{row.label}</LedgerRowHeader>
                      <LedgerCell numeric>{formatCount(row.newArtists)}</LedgerCell>
                      <LedgerCell numeric>{formatCount(row.newTracks)}</LedgerCell>
                    </LedgerRow>
                  ))}
                </LedgerBody>
              </Ledger>
            )}
          </Panel>
        </>
      )}
    </div>
  )
}

/** A chart-shaped placeholder, so the card does not resize when data lands. */
function ChartLoading({ label }: { label: string }): ReactElement {
  return (
    <div role="status" aria-live="polite" aria-busy="true" className="p-1" style={{ height: 260 }}>
      <span className="sr-only">{label}</span>
      <Skeleton className="h-full w-full" />
    </div>
  )
}
