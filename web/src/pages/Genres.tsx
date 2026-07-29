/**
 * Genres: what you listen to, not just who and what.
 *
 * A genre is not a fact Spotify hands over with a listen — it is read off the
 * artist, which arrives from the catalogue lookup on its own schedule. A fresh
 * instance, or one still working through an import, genuinely knows the genres
 * of almost nothing yet. The coverage sentence above the chart says so in
 * numbers, because an empty or half-populated chart with no explanation is
 * indistinguishable from a broken one — and the note under the table says the
 * other thing a first-time reader would otherwise conclude is a bug: the plays
 * column adds up to more than the range's total, because a track with three
 * genres counts once toward each of them.
 *
 * The bar chart and the stacked timeline are drawn from a single fetch of the
 * range's top eight genres — the server's own cap on a timeline's series —
 * taken from the head of the ranking (offset zero) rather than from whatever
 * page the ledger happens to be showing. The ledger below pages independently.
 * Deriving the chart's series from a paginated fetch would have reshaped both
 * charts every time somebody paged the table, which is the one thing a
 * ranked-and-paged view must never do to the ranking sitting above it.
 */

import type { ReactElement } from 'react'
import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { formatCount, formatDuration, formatPlural, formatRatio } from '../lib/format'
import type { GenresResponse, GenreTimelineResponse, Interval } from '../lib/types'
import {
  Button,
  EmptyState,
  ErrorState,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRank,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Pagination,
  Panel,
  RangePicker,
  Skeleton,
  SkeletonLedger,
  SkeletonText,
} from '../components/ui'
import { BarChart, ChartCard, GenreTimelineChart, INTERVAL_NOUN, MetricToggle } from '../components/charts'
import type { BarDatum, TimelineMetric } from '../components/charts'

/** One screenful of ledger, matching every other ranked list in the app. */
const PAGE_SIZE = 50

/**
 * How many genres the head-of-ranking fetch carries. Matches the server's own
 * `genreTimelineMaxSeries` — eight is where a stacked chart stops being
 * readable, so there is no point asking for more.
 */
const TOP_SERIES = 8

interface IntervalOption {
  id: Interval
  /** The nominal width the server estimates bucket counts with. */
  approxMs: number
}

/** The widest bucket, and the one every range is guaranteed to allow. */
const YEARLY: IntervalOption = { id: 'year', approxMs: 31_536_000_000 }

const INTERVALS: readonly IntervalOption[] = [
  { id: 'hour', approxMs: 3_600_000 },
  { id: 'day', approxMs: 86_400_000 },
  { id: 'week', approxMs: 604_800_000 },
  { id: 'month', approxMs: 2_592_000_000 },
  YEARLY,
]

/** The server's own cap on how many points one timeline response may carry. */
const MAX_BUCKETS = 1500

/** Above this the page picks a coarser default, so the plot stays readable. */
const COMFORTABLE_BUCKETS = 120

/** The same estimate the server makes, so the page never offers a 400. */
function bucketCount(durationMs: number, approxMs: number): number {
  return Math.floor(durationMs / approxMs) + 1
}

function readOffset(value: string | null): number {
  const parsed = Number.parseInt(value ?? '0', 10)
  if (!Number.isFinite(parsed) || parsed <= 0) return 0
  // Snapped to the page grid: a hand-edited `?offset=7` would otherwise put the
  // pagination control's own arithmetic permanently out of step with the rows.
  return Math.floor(parsed / PAGE_SIZE) * PAGE_SIZE
}

/** A chart-shaped placeholder, so the card does not resize when data lands. */
function ChartLoading({ label, height = 260 }: { label: string; height?: number }): ReactElement {
  return (
    <div role="status" aria-live="polite" aria-busy="true" className="p-1" style={{ height }}>
      <span className="sr-only">{label}</span>
      <Skeleton className="h-full w-full" />
    </div>
  )
}

export default function Genres(): ReactElement {
  const { range, label, timeZone } = useRange()
  const [metric, setMetric] = useState<TimelineMetric>('plays')

  // The ledger's page lives in the URL, exactly as it does on the other ranked
  // lists — not in local state — because `useRange`'s own writer already drops
  // a URL-held `offset` whenever the range changes. Local state would need its
  // own effect to get the same guarantee, and setting state synchronously
  // inside an effect is the cascading-render footgun the lint rule refuses.
  const [params, setParams] = useSearchParams()
  const offset = readOffset(params.get('offset'))
  const setOffset = (next: number): void => {
    setParams((current) => {
      const updated = new URLSearchParams(current)
      if (next <= 0) updated.delete('offset')
      else updated.set('offset', String(next))
      return updated
    })
  }

  const durationMs = Math.max(Date.parse(range.to) - Date.parse(range.from), 1)
  const allowed = useMemo(
    () => INTERVALS.filter((option) => bucketCount(durationMs, option.approxMs) <= MAX_BUCKETS),
    [durationMs],
  )
  const active =
    allowed.find((option) => bucketCount(durationMs, option.approxMs) <= COMFORTABLE_BUCKETS) ??
    allowed[0] ??
    YEARLY

  // The head of the ranking: fixed at offset zero, so the bar chart and the
  // timeline's series are stable no matter what page the ledger below is on.
  const genres = useQuery({
    queryKey: qk.genres(range, { limit: TOP_SERIES, offset: 0 }),
    queryFn: ({ signal }) =>
      api.get<GenresResponse>(
        '/stats/genres',
        { from: range.from, to: range.to, limit: TOP_SERIES, offset: 0 },
        signal,
      ),
  })

  /** The chart's series are the range's top eight, fixed so paging the table below does not reshape it. */
  const series = useMemo(
    () => (genres.data?.genres ?? []).slice(0, TOP_SERIES).map((g) => g.genre),
    [genres.data],
  )

  const timeline = useQuery({
    enabled: series.length > 0,
    queryKey: qk.genreTimeline(range, active.id, series),
    queryFn: ({ signal }) =>
      api.get<GenreTimelineResponse>(
        '/stats/genres/timeline',
        { from: range.from, to: range.to, interval: active.id, genre: series },
        signal,
      ),
  })

  // The full ranking, paginated on its own — independent of the head query
  // above, so paging it can never change what the charts are plotting.
  const ranking = useQuery({
    queryKey: qk.genres(range, { limit: PAGE_SIZE, offset }),
    queryFn: ({ signal }) =>
      api.get<GenresResponse>(
        '/stats/genres',
        { from: range.from, to: range.to, limit: PAGE_SIZE, offset },
        signal,
      ),
  })

  const coverage = genres.data?.coverage
  const noGenres = genres.isSuccess && (coverage?.covered ?? 0) === 0

  const status = genres.isPending
    ? `Loading genres for ${label.toLowerCase()}.`
    : genres.isError
      ? 'Your genre statistics could not be loaded.'
      : noGenres
        ? 'No genres are known yet for this range.'
        : coverage
          ? coverage.covered === coverage.total
            ? `Genres are known for all of your listening in ${label.toLowerCase()}.`
            : `Genres are known for ${formatRatio(coverage.covered, coverage.total)} of your listening in ${label.toLowerCase()}.`
          : ''

  const barData: BarDatum[] = useMemo(
    () =>
      (genres.data?.genres ?? []).map((g) => ({
        key: g.genre,
        label: g.genre,
        value: g.plays,
        hint: formatDuration(g.msPlayed),
      })),
    [genres.data],
  )

  const rankingRows = ranking.data?.genres ?? []
  const rankingTotal = ranking.data?.total ?? 0
  const pastEnd = ranking.isSuccess && rankingRows.length === 0 && rankingTotal > 0

  return (
    <div className="space-y-4">
      <PageHeader
        title="Genres"
        description={`Your listening by genre — ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {genres.isPending ? (
        <SkeletonText lines={1} className="max-w-md" />
      ) : genres.isSuccess && !noGenres && coverage ? (
        <p className="max-w-prose text-sm text-ink-muted">
          {coverage.covered === coverage.total
            ? 'Genres are known for all of your listening in this range.'
            : `Genres are known for ${formatRatio(coverage.covered, coverage.total)} of your listening in this range — ${formatCount(coverage.covered)} of ${formatCount(coverage.total)} plays.`}
        </p>
      ) : null}

      {genres.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={genres.error}
            title="Your genre statistics could not be loaded"
            onRetry={() => {
              void genres.refetch()
            }}
          />
        </Panel>
      ) : noGenres ? (
        <Panel padded={false}>
          <EmptyState
            title="No genres yet"
            description={
              <>
                Encore learns them from Spotify while it fills in your catalogue; check{' '}
                <Link to="/settings" className="text-lamp hover:underline">
                  Settings
                </Link>{' '}
                for progress.
              </>
            }
          />
        </Panel>
      ) : (
        <>
          <ChartCard
            title="Top genres"
            description="Your most played genres, ranked by plays, in this range."
          >
            {genres.isPending ? (
              <ChartLoading label="Loading top genres" height={TOP_SERIES * 30 + 34} />
            ) : (
              <BarChart
                data={barData}
                label="Top genres by plays"
                valueName="plays"
                busy={genres.isFetching}
                emptyDescription="Nothing was played in this range yet."
              />
            )}
          </ChartCard>

          <ChartCard
            title="Genres over time"
            description={`Your top genres, by ${INTERVAL_NOUN[active.id]}, in this range.`}
            control={<MetricToggle value={metric} onChange={setMetric} />}
          >
            {timeline.isPending ? (
              <ChartLoading label="Loading genre trends" height={280} />
            ) : (
              <GenreTimelineChart
                points={timeline.data?.points ?? []}
                genres={series}
                interval={active.id}
                timeZone={timeZone}
                metric={metric}
                busy={timeline.isFetching}
              />
            )}
          </ChartCard>

          <Panel
            title="Every genre"
            description="Highest first, across the whole range."
            padded={false}
          >
            {ranking.isPending ? (
              <SkeletonLedger rows={12} columns={3} />
            ) : ranking.isError ? (
              <ErrorState
                error={ranking.error}
                title="Your genre ranking could not be loaded"
                onRetry={() => {
                  void ranking.refetch()
                }}
              />
            ) : pastEnd ? (
              <EmptyState
                icon="genre"
                title="That page is past the end"
                description={`This range holds ${formatPlural(rankingTotal, 'genre')}, and this page starts after the last one.`}
                action={
                  <Button variant="primary" onClick={() => setOffset(0)}>
                    Back to the first page
                  </Button>
                }
              />
            ) : (
              <div
                className={
                  ranking.isFetching ? 'opacity-60 transition-opacity' : 'transition-opacity'
                }
              >
                <Ledger caption="Every genre played in this range, ranked by plays">
                  <LedgerHead>
                    <LedgerRow>
                      <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
                      <LedgerHeaderCell>Genre</LedgerHeaderCell>
                      <LedgerHeaderCell numeric aria-sort="descending">
                        Plays
                      </LedgerHeaderCell>
                      <LedgerHeaderCell numeric>Time</LedgerHeaderCell>
                    </LedgerRow>
                  </LedgerHead>
                  <LedgerBody>
                    {rankingRows.map((g, i) => (
                      <LedgerRow key={g.genre}>
                        <LedgerCell>
                          <LedgerRank rank={offset + i + 1} />
                        </LedgerCell>
                        <LedgerRowHeader className="text-sm font-normal tracking-normal normal-case">
                          {g.genre}
                        </LedgerRowHeader>
                        <LedgerCell numeric>{formatCount(g.plays)}</LedgerCell>
                        <LedgerCell numeric className="whitespace-nowrap">
                          {formatDuration(g.msPlayed)}
                        </LedgerCell>
                      </LedgerRow>
                    ))}
                  </LedgerBody>
                </Ledger>

                <Pagination
                  total={rankingTotal}
                  limit={PAGE_SIZE}
                  offset={offset}
                  onChange={setOffset}
                  label="Genres"
                  disabled={ranking.isFetching}
                  className="border-t border-seam"
                />
              </div>
            )}
          </Panel>

          <p className="text-xs text-ink-faint">
            A track counts toward each of its genres, so these add up to more than your total
            plays.
          </p>
        </>
      )}
    </div>
  )
}
