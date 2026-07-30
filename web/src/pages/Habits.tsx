/**
 * Habits: how you listened, as opposed to what you listened to.
 *
 * Every figure on this page is partial in a way the top lists and Genres are
 * not. Platform, country, shuffle, offline, incognito and the reason a play
 * ended are columns Spotify's *extended* streaming history export writes and
 * a live-synced play or a one-year account-data export never does — so an
 * instance built from sync alone has none of this, and a fresh import of the
 * one-year export still has none of it either. Showing four zero percentages
 * in that situation would read as "you never skip, shuffle, go offline or use
 * incognito," which is not a measurement, it is silence mistaken for a fact.
 * The coverage banner above everything else exists so the page says which
 * situation it is in before it says anything else.
 *
 * The four rates get their own coverage rather than one shared figure for the
 * whole page, because the columns are independent: an export can carry
 * `shuffle` but not `offline`, so the shuffle percentage and the skip
 * percentage are genuinely computed over different numbers of plays. Folding
 * them into a single "N% covered" would imply they agree when they need not.
 *
 * The three breakdowns (end reasons, platforms, countries) are drawn as
 * single-hue ranked bars rather than a stacked or pie chart, so the
 * palette's `SERIES_LIMIT` — the tested cap on how many categorical colours a
 * chart may seat before two of them collapse into each other for a
 * colour-blind reader — never comes into it: one bar chart is one colour, no
 * matter how many rows it ranks.
 */

import type { ReactElement } from 'react'
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { formatCount, formatPercent, formatRatio } from '../lib/format'
import type { PlaybackContextResponse, Rate, TasteResponse } from '../lib/types'
import {
  EmptyState,
  ErrorState,
  Icon,
  PageHeader,
  Panel,
  RangePicker,
  Skeleton,
  SkeletonText,
  Stat,
  StatGrid,
} from '../components/ui'
import { BarChart, ChartCard } from '../components/charts'
import type { BarDatum } from '../components/charts'

/**
 * How many countries the ranked bar chart shows. Unlike end reasons and
 * platforms — both closed sets the server already groups — a listener's
 * countries can run long, and a bar chart of forty rows stops being readable.
 * The cap matches `Discovery.tsx`'s own ranked-chart row count.
 */
const TOP_COUNTRIES = 12

const END_REASON_LABELS: Record<string, string> = {
  trackdone: 'Played to the end',
  fwdbtn: 'Skipped forward',
  backbtn: 'Went back',
  endplay: 'Stopped',
  logout: 'Signed out',
  remote: 'Changed remotely',
  trackerror: 'Playback error',
  unknown: 'Unknown',
  other: 'Other',
}

const PLATFORM_LABELS: Record<string, string> = {
  android: 'Android',
  ios: 'iOS',
  windows: 'Windows',
  macos: 'macOS',
  linux: 'Linux',
  web: 'Web player',
  cast: 'Cast',
  partner: 'Partner device',
  other: 'Other',
}

/**
 * Looks a raw key up in a label map, falling back to the key itself. The
 * server's own end-reason values pass through unmapped for anything it does
 * not recognise, so an unknown key must still be shown, not dropped.
 */
function labelFor(map: Record<string, string>, key: string): string {
  return map[key] ?? key
}

function toBarData(slices: { key: string; plays: number }[], map?: Record<string, string>): BarDatum[] {
  return slices.map((slice) => ({
    key: slice.key,
    label: map ? labelFor(map, slice.key) : slice.key,
    value: slice.plays,
  }))
}

/** What a rate's own hint reads while its query is still in flight or done. */
function rateHint(rate: Rate | undefined, pending: boolean): string {
  if (pending) return 'Looking for it'
  if (!rate || rate.total <= 0) return 'Not known for this range'
  return `${formatRatio(rate.covered, rate.total)} of plays in this range carry this detail`
}

/** What a taste score's own hint reads while its query is still in flight or done. */
function tasteHint(rate: Rate | undefined, pending: boolean): string {
  if (pending) return 'Looking for it'
  if (!rate || rate.total <= 0) return 'Not known for this range'
  return `Known for ${formatRatio(rate.covered, rate.total)} of your listening in this range`
}

/**
 * A release-lag figure is a count of years, not a ratio — `formatPercent`
 * would turn "8.4 years" into a nonsense percentage.
 */
function formatYears(value: number): string {
  if (!Number.isFinite(value)) return '0.0'
  return value.toFixed(1)
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

export default function Habits(): ReactElement {
  const { range, label } = useRange()

  const context = useQuery({
    queryKey: qk.playbackContext(range),
    queryFn: ({ signal }) =>
      api.get<PlaybackContextResponse>(
        '/stats/context',
        { from: range.from, to: range.to },
        signal,
      ),
  })

  const data = context.data
  // Zero, not merely partial: *every* extended-export column is absent from
  // this range, so every rate below would be a fabricated zero rather than a
  // measurement. Gating on `skipRate` alone — one column of six, independent
  // of the rest per this file's own doc comment above — collapsed the page
  // to "No playback detail yet" whenever an export happened to omit
  // `reason_end` while still carrying `platform`, `shuffle` and the rest.
  const noContext =
    context.isSuccess &&
    (data?.skipRate.covered ?? 0) === 0 &&
    (data?.shuffleRate.covered ?? 0) === 0 &&
    (data?.offlineRate.covered ?? 0) === 0 &&
    (data?.incognitoRate.covered ?? 0) === 0 &&
    (data?.platformCoverage.covered ?? 0) === 0 &&
    (data?.countryCoverage.covered ?? 0) === 0

  const taste = useQuery({
    queryKey: qk.taste(range),
    queryFn: ({ signal }) =>
      api.get<TasteResponse>('/stats/taste', { from: range.from, to: range.to }, signal),
  })

  const status = context.isPending
    ? `Loading listening habits for ${label.toLowerCase()}.`
    : context.isError
      ? 'Your listening habits could not be loaded.'
      : noContext
        ? 'No playback detail is known yet for this range.'
        : data
          ? `How a play ended is known for ${formatRatio(data.skipRate.covered, data.skipRate.total)} of your listening in ${label.toLowerCase()}.`
          : ''

  const endReasonData = useMemo(
    () => toBarData(data?.endReasons ?? [], END_REASON_LABELS),
    [data],
  )
  const platformData = useMemo(() => toBarData(data?.platforms ?? [], PLATFORM_LABELS), [data])
  const countries = useMemo(() => data?.countries ?? [], [data])
  const countryData = useMemo(() => toBarData(countries.slice(0, TOP_COUNTRIES)), [countries])
  // Only worth naming the cut-off when it actually trims something: "the top
  // 12 of 3" would tell a reader with three countries that a limit applies to
  // them when it never bit.
  const countriesDescription =
    countries.length > TOP_COUNTRIES
      ? `Connection countries, from plays that recorded one — the top ${TOP_COUNTRIES} of ${formatCount(countries.length)}.`
      : 'Connection countries, from plays that recorded one.'

  return (
    <div className="space-y-4">
      <PageHeader
        title="Habits"
        description={`How you listened, rather than what you listened to — ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {context.isPending ? (
        <SkeletonText lines={2} className="max-w-2xl" />
      ) : context.isSuccess && !noContext && data ? (
        <Panel bodyClassName="flex items-start gap-3 p-4">
          <Icon name="info" size={20} className="mt-0.5 shrink-0 text-ink-muted" />
          <p className="text-sm text-ink">
            Based on the {formatRatio(data.skipRate.covered, data.skipRate.total)} of your
            listening that records how a play ended. Only Spotify's extended streaming history
            export records it — plays recorded live by Encore, and plays from the one-year
            account-data export, do not.
          </p>
        </Panel>
      ) : null}

      {context.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={context.error}
            title="Your listening habits could not be loaded"
            onRetry={() => {
              void context.refetch()
            }}
          />
        </Panel>
      ) : noContext ? (
        <Panel padded={false}>
          <EmptyState
            icon="habits"
            title="No playback detail yet"
            description={
              <>
                Skip, shuffle, offline and incognito are recorded only by Spotify's extended
                streaming history export — an instance built from live sync, or from the
                shorter one-year account-data export, has none of it yet. Bring one in from{' '}
                <Link to="/imports" className="text-lamp hover:underline">
                  Imports
                </Link>{' '}
                to see how you listen, not just what.
              </>
            }
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={4}>
            <Stat
              label="Skip rate"
              value={formatPercent(data?.skipRate.value ?? 0)}
              loading={context.isPending}
              hint={rateHint(data?.skipRate, context.isPending)}
            />
            <Stat
              label="Shuffle rate"
              value={formatPercent(data?.shuffleRate.value ?? 0)}
              loading={context.isPending}
              hint={rateHint(data?.shuffleRate, context.isPending)}
            />
            <Stat
              label="Offline rate"
              value={formatPercent(data?.offlineRate.value ?? 0)}
              loading={context.isPending}
              hint={rateHint(data?.offlineRate, context.isPending)}
            />
            <Stat
              label="Incognito rate"
              value={formatPercent(data?.incognitoRate.value ?? 0)}
              loading={context.isPending}
              hint={rateHint(data?.incognitoRate, context.isPending)}
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <ChartCard
              title="How plays ended"
              description="Every play that recorded an end reason, in this range."
            >
              {context.isPending ? (
                <ChartLoading label="Loading end reasons" />
              ) : (
                <BarChart
                  data={endReasonData}
                  label="Plays by end reason"
                  valueName="plays"
                  slot={0}
                  busy={context.isFetching}
                  emptyDescription="No play in this range recorded how it ended."
                />
              )}
            </ChartCard>

            <ChartCard
              title="Where you listened"
              description="Platform families, from plays that recorded one."
            >
              {context.isPending ? (
                <ChartLoading label="Loading platforms" />
              ) : (
                <BarChart
                  data={platformData}
                  label="Plays by platform"
                  valueName="plays"
                  slot={1}
                  busy={context.isFetching}
                  emptyDescription="No play in this range recorded a platform."
                />
              )}
            </ChartCard>
          </div>

          <ChartCard title="Countries" description={countriesDescription}>
            {context.isPending ? (
              <ChartLoading label="Loading countries" />
            ) : (
              <BarChart
                data={countryData}
                label="Plays by country"
                valueName="plays"
                slot={2}
                busy={context.isFetching}
                emptyDescription="No play in this range recorded a country."
              />
            )}
          </ChartCard>

          <p className="text-xs text-ink-faint">
            A skip is a track ended with the forward button. Going back is counted separately.
          </p>
        </>
      )}

      {/*
       * Taste: what kind of catalogue this is, independent of the playback-
       * context columns above. Obscurity and release lag come from artist
       * popularity and album release dates — enrichment data that a sync-only
       * or account-data-only instance can have plenty of even when every rate
       * above reads "not known" — so this section renders on its own rather
       * than living inside the `noContext` gate. It is also the taste
       * statistic the Dashboard's obscurity card points here to find; before
       * this section existed, that link led to a page with none.
       */}
      {taste.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={taste.error}
            title="Taste could not be loaded"
            onRetry={() => {
              void taste.refetch()
            }}
          />
        </Panel>
      ) : (
        <Panel
          title="Taste"
          description="How mainstream your listening is, and how old the music tends to be."
          padded={false}
        >
          {/* `StatGrid` would draw a second panel border inside this one, so
              the seamed grid it is built from is repeated here without the
              frame — the same technique the dashboard's own "Also worth
              knowing" panel uses for the same reason. */}
          <div className="grid gap-px bg-seam sm:grid-cols-2 [&>*]:bg-panel">
            <Stat
              label="Obscurity"
              value={formatCount(taste.data?.obscurity.value ?? 0)}
              suffix="of 100"
              loading={taste.isPending}
              hint={tasteHint(taste.data?.obscurity, taste.isPending)}
            />
            <Stat
              label="Release lag"
              value={formatYears(taste.data?.releaseLag.value ?? 0)}
              suffix="years old"
              loading={taste.isPending}
              hint={tasteHint(taste.data?.releaseLag, taste.isPending)}
            />
          </div>
        </Panel>
      )}
    </div>
  )
}
