/**
 * The dashboard: what a signed-in person sees first.
 *
 * Every panel fetches on its own and shows its own skeleton, so one slow
 * analytic query — the heatmap-sized ones can be slow on a decade of listens —
 * never blanks the page. The figures all come from the shared URL range, so the
 * address bar is the state and a link to this page shows the same thing to
 * whoever opens it.
 *
 * The one branch worth naming: a person with no listens at all does not get an
 * empty grid, they get the import instructions. That is the first screen after
 * signing in for anyone who has just installed Encore, and an empty dashboard
 * would be a dead end rather than a start.
 */

import type { ReactElement, ReactNode } from 'react'
import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { ALL_TIME_START, useRange } from '../lib/range'
import type { DateRange } from '../lib/range'
import {
  EMPTY,
  formatCount,
  formatDateTime,
  formatDuration,
  formatPlural,
  formatRatio,
  formatRelative,
  formatSigned,
  rankChange,
} from '../lib/format'
import type {
  ArtistRef,
  CompareResponse,
  GenresResponse,
  HistoryItem,
  HistoryResponse,
  RepartitionBucket,
  StatsExtras,
  Summary,
  TasteResponse,
  TimelineResponse,
  TopArtists,
  TopEntry,
  TopTracks,
  TrackRef,
} from '../lib/types'
import {
  ButtonLink,
  Chip,
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
  Panel,
  RangeLink,
  RangePicker,
  Skeleton,
  SkeletonLedger,
  Stat,
  StatGrid,
} from '../components/ui'
import {
  BarChart,
  ChartCard,
  HourChart,
  MetricToggle,
  ShareBar,
  Sparkline,
  TimelineChart,
  WeekdayChart,
} from '../components/charts'
import type { BarDatum, TimelineMetric } from '../components/charts'

const DAY_MS = 86_400_000

/** Five is what fits beside another five without either becoming a scroll. */
const TOP_PAGE = { limit: 5, offset: 0 }

/** Enough recent plays to fill the strip on a wide screen. */
const RECENT_LIMIT = 12

/**
 * The top-genres bar chart's fixed height, so the card is the same size
 * whether it is showing a skeleton, five bars or the empty state — never a
 * card that grows or shrinks as its query settles. Matches the row-height
 * arithmetic `Genres.tsx` tunes its own top-genres chart to.
 */
const GENRE_CHART_HEIGHT = TOP_PAGE.limit * 30 + 34

/**
 * The obscurity panel's minimum body height, pinned to the tallest of its
 * three *routine* states — the loading skeleton, the populated `Stat`, and
 * the empty state shown while coverage is still zero — so the panel does not
 * resize as coverage crosses from zero to nonzero while enrichment runs.
 * `Stat`, `EmptyState` and `ErrorState` have no `height` prop the way
 * `BarChart` does, so the constant is applied to a wrapping element instead
 * of any of those shared components.
 *
 * `ErrorState` is deliberately left free to grow past this: it is taller
 * than the other three (its retry button adds height none of them have), a
 * failed request is rare, and it already commands attention on its own —
 * a resize there costs far less than one during ordinary enrichment.
 */
const OBSCURITY_MIN_HEIGHT = 210

export default function Dashboard(): ReactElement {
  const { range, label, timeZone } = useRange()
  const [metric, setMetric] = useState<TimelineMetric>('plays')

  /** The equal-length period immediately before this one, if there is one. */
  const previous = useMemo<DateRange | null>(() => {
    const from = Date.parse(range.from)
    const to = Date.parse(range.to)
    if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) return null
    const start = from - (to - from)
    // "All time" already reaches back further than Spotify existed, so there is
    // nothing behind it to compare against and the delta would be theatre.
    if (start < Date.parse(ALL_TIME_START)) return null
    return { from: new Date(start).toISOString(), to: range.from }
  }, [range])

  const everRange = useMemo<DateRange>(() => ({ from: ALL_TIME_START, to: range.to }), [range.to])

  const summary = useQuery({
    queryKey: qk.summary(range),
    queryFn: ({ signal }) =>
      api.get<Summary>('/stats/summary', { from: range.from, to: range.to }, signal),
  })

  const listens = summary.data?.listens ?? 0
  const rangeIsEmpty = summary.isSuccess && listens <= 0

  // Only asked when the range came back empty: it is the difference between
  // "nothing happened this month" and "nothing has ever been imported".
  const ever = useQuery({
    queryKey: qk.summary(everRange),
    queryFn: ({ signal }) =>
      api.get<Summary>('/stats/summary', { from: everRange.from, to: everRange.to }, signal),
    enabled: rangeIsEmpty,
  })

  const compare = useQuery({
    queryKey: qk.compare(range, previous ?? range),
    queryFn: ({ signal }) =>
      api.get<CompareResponse>(
        '/stats/compare',
        {
          aFrom: range.from,
          aTo: range.to,
          bFrom: previous?.from,
          bTo: previous?.to,
        },
        signal,
      ),
    enabled: previous !== null,
  })

  const timeline = useQuery({
    queryKey: qk.timeline(range, null),
    queryFn: ({ signal }) =>
      api.get<TimelineResponse>('/stats/timeline', { from: range.from, to: range.to }, signal),
  })

  const topTracks = useQuery({
    queryKey: qk.top('tracks', range, TOP_PAGE),
    queryFn: ({ signal }) =>
      api.get<TopTracks>(
        '/stats/top/tracks',
        { from: range.from, to: range.to, ...TOP_PAGE },
        signal,
      ),
  })

  const topArtists = useQuery({
    queryKey: qk.top('artists', range, TOP_PAGE),
    queryFn: ({ signal }) =>
      api.get<TopArtists>(
        '/stats/top/artists',
        { from: range.from, to: range.to, ...TOP_PAGE },
        signal,
      ),
  })

  const hours = useQuery({
    queryKey: qk.repartition('hour', range),
    queryFn: ({ signal }) =>
      api.get<RepartitionBucket[]>(
        '/stats/repartition/hour',
        { from: range.from, to: range.to },
        signal,
      ),
  })

  const weekdays = useQuery({
    queryKey: qk.repartition('weekday', range),
    queryFn: ({ signal }) =>
      api.get<RepartitionBucket[]>(
        '/stats/repartition/weekday',
        { from: range.from, to: range.to },
        signal,
      ),
  })

  const recent = useQuery({
    queryKey: qk.history(range, RECENT_LIMIT),
    queryFn: ({ signal }) =>
      api.get<HistoryResponse>(
        '/history',
        { from: range.from, to: range.to, limit: RECENT_LIMIT },
        signal,
      ),
  })

  const extras = useQuery({
    queryKey: qk.extras(range),
    queryFn: ({ signal }) =>
      api.get<StatsExtras>('/stats/extras', { from: range.from, to: range.to }, signal),
  })

  // The head of the genre ranking, exactly like `topTracks`/`topArtists`
  // above: the same five-row page, so a card beside them behaves the same way.
  const genres = useQuery({
    queryKey: qk.genres(range, TOP_PAGE),
    queryFn: ({ signal }) =>
      api.get<GenresResponse>(
        '/stats/genres',
        { from: range.from, to: range.to, ...TOP_PAGE },
        signal,
      ),
  })

  const taste = useQuery({
    queryKey: qk.taste(range),
    queryFn: ({ signal }) =>
      api.get<TasteResponse>('/stats/taste', { from: range.from, to: range.to }, signal),
  })

  const buckets = timeline.data?.buckets ?? []
  const interval = timeline.data?.interval ?? 'day'
  const rangeDays = Math.max(
    1,
    Math.round((Date.parse(range.to) - Date.parse(range.from)) / DAY_MS) || 1,
  )
  const previousLength = previous
    ? formatPlural(
        Math.max(1, Math.round((Date.parse(previous.to) - Date.parse(previous.from)) / DAY_MS)),
        'day',
      )
    : ''

  const description = `Your listening, ${label.toLowerCase()}.`

  // --- the three whole-page states ----------------------------------------

  if (summary.isPending || (rangeIsEmpty && ever.isPending)) {
    return (
      <Shell description={description} status={`Loading your dashboard for ${label}.`}>
        <div className="panel h-32" aria-hidden="true" />
        <div className="panel h-72" aria-hidden="true" />
      </Shell>
    )
  }

  if (summary.isError) {
    return (
      <Shell description={description} status="The dashboard could not be loaded.">
        <Panel padded={false}>
          <ErrorState
            error={summary.error}
            title="The dashboard could not be loaded"
            onRetry={() => {
              void summary.refetch()
            }}
          />
        </Panel>
      </Shell>
    )
  }

  if (rangeIsEmpty && ever.isSuccess && (ever.data?.listens ?? 0) <= 0) {
    return (
      <Shell description="Nothing has been imported yet." status="No listening history yet." bare>
        <Panel padded={false}>
          <EmptyState
            icon="import"
            title="Import your history"
            description={
              <>
                Spotify offers two exports. <strong className="font-semibold">Account data</strong>{' '}
                arrives in a few days and covers roughly the last year;{' '}
                <strong className="font-semibold">extended streaming history</strong> takes a few
                weeks and covers everything you have ever played. Encore reads either, and both
                together.
              </>
            }
            action={
              <ButtonLink to="/imports" variant="primary">
                Import your history
              </ButtonLink>
            }
          />
        </Panel>
      </Shell>
    )
  }

  if (rangeIsEmpty) {
    return (
      <Shell description={description} status={`No listens in ${label.toLowerCase()}.`}>
        <Panel padded={false}>
          <EmptyState
            title="No listens in this range"
            description="You have listening history, just none between these dates. Widen the range above, or look at all time."
            action={
              <ButtonLink to="/history" variant="primary">
                Browse your history
              </ButtonLink>
            }
          />
        </Panel>
      </Shell>
    )
  }

  // --- the dashboard proper -----------------------------------------------

  const totals = summary.data
  const delta = compare.data?.delta
  const previousActiveDays = compare.data?.b?.summary?.activeDays
  const activeDaysChange =
    totals && typeof previousActiveDays === 'number' ? totals.activeDays - previousActiveDays : null

  /** What the tiles can honestly say about the preceding period right now. */
  const comparison: ComparisonState =
    previous === null ? 'none' : compare.isPending ? 'loading' : compare.isError ? 'error' : 'ready'

  // Genre coverage is zero exactly when no listen in range has a resolved
  // artist yet, which is also exactly when the top-five fetch below is empty —
  // this is what decides *which* empty state the chart shows, never the chart's
  // own row count, so a genuinely quiet range and an unenriched one read
  // differently even though both hand the chart zero rows.
  const genreCoverage = genres.data?.coverage
  const noGenres = genres.isSuccess && (genreCoverage?.covered ?? 0) === 0
  const genreBarData: BarDatum[] = (genres.data?.genres ?? []).map((entry) => ({
    key: entry.genre,
    label: entry.genre,
    value: entry.plays,
    hint: formatDuration(entry.msPlayed),
  }))

  // Same shape of gate as `noGenres`: a genuine obscurity of 0 with plays
  // behind it is "deep cuts", a real reading, and must not be confused with
  // nothing having been measured at all.
  const obscurity = taste.data?.obscurity
  const noObscurity = taste.isSuccess && (obscurity?.covered ?? 0) === 0

  return (
    <Shell
      description={description}
      status={`Dashboard ready for ${label.toLowerCase()}: ${formatPlural(listens, 'listen')}.`}
    >
      <StatGrid columns={3}>
        <Stat
          label="Listens"
          value={formatCount(totals?.listens ?? 0)}
          lamp
          hint={
            <>
              <Delta
                state={comparison}
                change={delta?.listens ?? null}
                format={formatSigned}
                period={previousLength}
              />
              {buckets.length > 1 ? (
                <Sparkline
                  className="mt-1 block"
                  values={buckets.map((bucket) => bucket.plays)}
                  label="Listens per bucket over the range"
                  width={88}
                  height={18}
                />
              ) : null}
            </>
          }
        />
        <Stat
          label="Listening time"
          value={formatDuration(totals?.msPlayed ?? 0)}
          hint={
            <>
              <Delta
                state={comparison}
                change={delta?.msPlayed ?? null}
                format={signedDuration}
                period={previousLength}
              />
              {buckets.length > 1 ? (
                <Sparkline
                  className="mt-1 block"
                  values={buckets.map((bucket) => bucket.msPlayed)}
                  label="Listening time per bucket over the range"
                  format={formatDuration}
                  width={88}
                  height={18}
                />
              ) : null}
            </>
          }
        />
        <Stat
          label="Active days"
          value={formatCount(totals?.activeDays ?? 0)}
          suffix={`of ${formatCount(rangeDays)}`}
          meter={(totals?.activeDays ?? 0) / rangeDays}
          hint={
            <Delta
              state={comparison}
              change={activeDaysChange}
              format={formatSigned}
              period={previousLength}
            />
          }
        />
        <Stat
          label="Distinct tracks"
          value={formatCount(totals?.distinctTracks ?? 0)}
          hint={
            <Delta
              state={comparison}
              change={delta?.distinctTracks ?? null}
              format={formatSigned}
              period={previousLength}
            />
          }
        />
        <Stat
          label="Distinct artists"
          value={formatCount(totals?.distinctArtists ?? 0)}
          hint={
            <Delta
              state={comparison}
              change={delta?.distinctArtists ?? null}
              format={formatSigned}
              period={previousLength}
            />
          }
        />
        <Stat
          label="Distinct albums"
          value={formatCount(totals?.distinctAlbums ?? 0)}
          hint={
            <Delta
              state={comparison}
              change={delta?.distinctAlbums ?? null}
              format={formatSigned}
              period={previousLength}
            />
          }
        />
      </StatGrid>

      <ChartCard
        title="Listening over time"
        description={`One point per ${interval}, in your timezone.`}
        control={<MetricToggle value={metric} onChange={setMetric} />}
      >
        {timeline.isPending ? (
          <ChartLoading height={260} label="Loading the timeline" />
        ) : timeline.isError ? (
          <ErrorState
            error={timeline.error}
            title="The timeline could not be loaded"
            onRetry={() => {
              void timeline.refetch()
            }}
          />
        ) : (
          <TimelineChart
            buckets={buckets}
            interval={interval}
            timeZone={timeZone}
            metric={metric}
            busy={timeline.isFetching}
          />
        )}
      </ChartCard>

      <div className="grid gap-4 lg:grid-cols-2">
        <Panel
          title="Top tracks"
          description="Most played in this range"
          padded={false}
          actions={
            <Link to="/tracks" className="text-xs text-ink-muted hover:text-lamp">
              All tracks
            </Link>
          }
        >
          {topTracks.isPending ? (
            <SkeletonLedger rows={5} columns={3} />
          ) : topTracks.isError ? (
            <ErrorState
              error={topTracks.error}
              title="Top tracks could not be loaded"
              onRetry={() => {
                void topTracks.refetch()
              }}
            />
          ) : (topTracks.data?.items ?? []).length === 0 ? (
            <EmptyState
              title="No tracks in this range"
              description="Widen the date range to see what you played."
            />
          ) : (
            <TopLedger
              caption="Your five most played tracks in this range"
              what="Track"
              rows={(topTracks.data?.items ?? []).map((entry) => ({
                key: entry.entity.id,
                to: `/tracks/${entry.entity.id}`,
                name: entry.entity.name,
                sub: entry.entity.artists.map((artist) => artist.name).join(', '),
                plays: entry.plays,
                rank: entry.rank,
                previousRank: entry.previousRank,
              }))}
            />
          )}
        </Panel>

        <Panel
          title="Top artists"
          description="Most played in this range"
          padded={false}
          actions={
            <Link to="/artists" className="text-xs text-ink-muted hover:text-lamp">
              All artists
            </Link>
          }
        >
          {topArtists.isPending ? (
            <SkeletonLedger rows={5} columns={3} />
          ) : topArtists.isError ? (
            <ErrorState
              error={topArtists.error}
              title="Top artists could not be loaded"
              onRetry={() => {
                void topArtists.refetch()
              }}
            />
          ) : (topArtists.data?.items ?? []).length === 0 ? (
            <EmptyState
              title="No artists in this range"
              description="Widen the date range to see who you played."
            />
          ) : (
            <>
              <TopLedger
                caption="Your five most played artists in this range"
                what="Artist"
                rows={(topArtists.data?.items ?? []).map((entry) => ({
                  key: entry.entity.id,
                  to: `/artists/${entry.entity.id}`,
                  name: entry.entity.name,
                  plays: entry.plays,
                  rank: entry.rank,
                  previousRank: entry.previousRank,
                }))}
              />
              <LeaderShare entry={topArtists.data?.items?.[0]} total={totals?.msPlayed ?? 0} />
            </>
          )}
        </Panel>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <ChartCard title="Hour of day" description="When you listen, in your timezone">
          {hours.isPending ? (
            <ChartLoading height={220} label="Loading the hour repartition" />
          ) : hours.isError ? (
            <ErrorState
              error={hours.error}
              title="The hour repartition could not be loaded"
              onRetry={() => {
                void hours.refetch()
              }}
            />
          ) : (
            <HourChart buckets={hours.data ?? []} busy={hours.isFetching} />
          )}
        </ChartCard>

        <ChartCard title="Day of the week" description="Which days carry your listening">
          {weekdays.isPending ? (
            <ChartLoading height={220} label="Loading the weekday repartition" />
          ) : weekdays.isError ? (
            <ErrorState
              error={weekdays.error}
              title="The weekday repartition could not be loaded"
              onRetry={() => {
                void weekdays.refetch()
              }}
            />
          ) : (
            <WeekdayChart buckets={weekdays.data ?? []} busy={weekdays.isFetching} />
          )}
        </ChartCard>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <ChartCard
          title="Top genres"
          description="Your five most played genres in this range."
          control={
            <Link to="/genres" className="text-xs text-ink-muted hover:text-lamp">
              All genres
            </Link>
          }
        >
          {genres.isPending ? (
            <ChartLoading height={GENRE_CHART_HEIGHT} label="Loading top genres" />
          ) : genres.isError ? (
            <ErrorState
              error={genres.error}
              title="Top genres could not be loaded"
              onRetry={() => {
                void genres.refetch()
              }}
            />
          ) : (
            <>
              <BarChart
                data={genreBarData}
                label="Top genres by plays"
                valueName="plays"
                height={GENRE_CHART_HEIGHT}
                busy={genres.isFetching}
                emptyDescription={
                  noGenres
                    ? 'Not known yet — Encore learns genres from your artists as enrichment catches up.'
                    : 'Nothing was played in this range yet.'
                }
              />
              {/* Coverage in prose, not a tooltip — matches `Genres.tsx`'s own
                  sentence so the two never disagree about the same number. Held
                  back exactly when `noGenres` is, since the chart's own empty
                  state already explains a zero in that case. */}
              {!noGenres && genreCoverage ? (
                <p className="px-1 pb-1 text-xs text-ink-faint">
                  {genreCoverage.covered === genreCoverage.total
                    ? 'Genres are known for all of your listening in this range.'
                    : `Genres are known for ${formatRatio(genreCoverage.covered, genreCoverage.total)} of your listening in this range — ${formatCount(genreCoverage.covered)} of ${formatCount(genreCoverage.total)} plays.`}
                </p>
              ) : null}
            </>
          )}
        </ChartCard>

        <Panel
          title="Obscurity"
          description="How mainstream your listening is, in this range."
          padded={false}
          actions={
            <Link to="/habits" className="text-xs text-ink-muted hover:text-lamp">
              Habits
            </Link>
          }
        >
          <div className="flex flex-col justify-center" style={{ minHeight: OBSCURITY_MIN_HEIGHT }}>
            {taste.isPending ? (
              <div className="p-4">
                <Skeleton className="h-3 w-24" />
                <Skeleton className="mt-3 h-9 w-20" />
                <Skeleton className="mt-3 h-3 w-40" />
              </div>
            ) : taste.isError ? (
              <ErrorState
                error={taste.error}
                title="Obscurity could not be loaded"
                onRetry={() => {
                  void taste.refetch()
                }}
              />
            ) : noObscurity ? (
              <EmptyState
                title="Not known yet"
                description="Obscurity is worked out from your artists' popularity, once enrichment has caught up."
              />
            ) : (
              <Stat
                label="Obscurity"
                value={formatCount(obscurity?.value ?? 0)}
                suffix="of 100"
                meter={(obscurity?.value ?? 0) / 100}
                hint={
                  <>
                    {obscurityBand(obscurity?.value ?? 0)} — known for{' '}
                    {formatRatio(obscurity?.covered ?? 0, obscurity?.total ?? 0)} of your listening in
                    this range
                  </>
                }
              />
            )}
          </div>
        </Panel>
      </div>

      <Panel
        title="Recently played"
        description="The latest listens in this range"
        padded={false}
        actions={
          <Link to="/history" className="text-xs text-ink-muted hover:text-lamp">
            All history
          </Link>
        }
      >
        {recent.isPending ? (
          <div className="flex gap-2 p-3">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-16 w-52 shrink-0" />
            ))}
          </div>
        ) : recent.isError ? (
          <ErrorState
            error={recent.error}
            title="Recent plays could not be loaded"
            onRetry={() => {
              void recent.refetch()
            }}
          />
        ) : (recent.data?.items ?? []).length === 0 ? (
          <EmptyState
            title="Nothing played in this range"
            description="Recent plays arrive from Spotify on the next sync, or from an import."
          />
        ) : (
          <RecentStrip items={recent.data?.items ?? []} timeZone={timeZone} />
        )}
      </Panel>

      <Panel title="Also worth knowing" description="Odds and ends about this range" padded={false}>
        {extras.isPending ? (
          <div className="grid gap-px bg-seam sm:grid-cols-3 [&>*]:bg-panel">
            {Array.from({ length: 3 }, (_, i) => (
              <div key={i} className="p-4">
                <Skeleton className="h-3 w-24" />
                <Skeleton className="mt-3 h-9 w-20" />
              </div>
            ))}
          </div>
        ) : extras.isError ? (
          <ErrorState
            error={extras.error}
            title="These figures could not be loaded"
            onRetry={() => {
              void extras.refetch()
            }}
          />
        ) : (
          // `StatGrid` would draw a second panel border inside this one, so the
          // seamed grid it is built from is repeated here without the frame.
          <div className="grid gap-px bg-seam sm:grid-cols-3 [&>*]:bg-panel">
            <Stat
              label="Different artists"
              value={formatCount(extras.data?.differentArtists ?? 0)}
              hint="Counted across the whole range"
            />
            <Stat
              label="Average release year"
              value={formatYear(extras.data?.averageAlbumReleaseYear ?? null)}
              hint="Of the albums you played"
            />
            <Stat
              label="Artists per track"
              value={formatAverage(extras.data?.averageArtistsPerTrack ?? 0)}
              hint="Collaborations push this above one"
            />
          </div>
        )}
      </Panel>
    </Shell>
  )
}

// --- page furniture --------------------------------------------------------

/**
 * The page's one h1 and its live region, in every branch.
 *
 * Keeping the header outside the branching is what guarantees the heading, the
 * document title and the range picker survive an error or an empty range —
 * a page that loses its own title when a query fails is disorienting.
 */
function Shell({
  description,
  status,
  bare = false,
  children,
}: {
  description: string
  status: string
  /** Drops the range picker, for the state where no range would help. */
  bare?: boolean
  children: ReactNode
}): ReactElement {
  return (
    <div className="space-y-4">
      <PageHeader
        title="Dashboard"
        description={description}
        actions={bare ? undefined : <RangePicker />}
      />
      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>
      {children}
    </div>
  )
}

/** A chart-shaped placeholder, so the card does not resize when data lands. */
function ChartLoading({ height, label }: { height: number; label: string }): ReactElement {
  return (
    <div role="status" aria-live="polite" aria-busy="true" style={{ height }} className="p-1">
      <span className="sr-only">{label}</span>
      <Skeleton className="h-full w-full" />
    </div>
  )
}

type ComparisonState = 'none' | 'loading' | 'error' | 'ready'

/**
 * A change against the preceding period.
 *
 * Deliberately not coloured green and red: listening more is not success and
 * listening less is not failure, and dressing a neutral number in status colours
 * would say otherwise. The sign carries the direction, and a word carries it for
 * anyone who cannot see the sign.
 */
function Delta({
  state,
  change,
  format,
  period,
}: {
  state: ComparisonState
  change: number | null
  format: (value: number) => string
  period: string
}): ReactElement {
  const named = period ? `the previous ${period}` : 'the previous period'

  if (state === 'none') return <span>No earlier period to compare with</span>
  if (state === 'loading') return <span>Comparing with {named}…</span>
  if (state === 'error' || change === null) return <span>Comparison unavailable</span>
  if (change === 0) return <span>No change on {named}</span>

  return (
    <span className="inline-flex flex-wrap items-baseline gap-1">
      <span className="tabular text-ink-muted">{format(change)}</span>
      <span className="sr-only">{change > 0 ? 'more' : 'fewer'}</span>
      <span>on {named}</span>
    </span>
  )
}

/** `+4h 12m`, the duration equivalent of `formatSigned`. */
function signedDuration(value: number): string {
  if (!Number.isFinite(value) || value === 0) return formatDuration(0)
  return `${value > 0 ? '+' : '-'}${formatDuration(Math.abs(value))}`
}

/**
 * A year takes no thousands separator, so `formatCount` — which would render
 * 1998 as "1,998" — is the wrong tool for exactly this one figure.
 */
function formatYear(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return EMPTY
  return Math.round(value).toFixed(0)
}

/**
 * An average with one decimal. `formatCount` rounds to whole numbers, which
 * would turn "1.8 artists per track" into "2" and lose the point of the figure.
 */
function formatAverage(value: number): string {
  if (!Number.isFinite(value)) return EMPTY
  return value.toFixed(1)
}

/**
 * The obscurity score in a word. The score is already Spotify's own artist
 * popularity, 0-100 and play-weighted, so these bands read off it directly —
 * there is no fraction to convert first.
 */
function obscurityBand(value: number): string {
  if (value >= 75) return 'chart music'
  if (value >= 50) return 'broadly popular'
  if (value >= 25) return 'off the beaten track'
  return 'deep cuts'
}

// --- panels ----------------------------------------------------------------

interface TopRow {
  key: string
  to: string
  name: string
  sub?: string
  plays: number
  rank: number
  previousRank: number | null
}

/** The five-row leaderboard both top panels are made of. */
function TopLedger({
  caption,
  what,
  rows,
}: {
  caption: string
  what: string
  rows: TopRow[]
}): ReactElement {
  return (
    <Ledger caption={caption}>
      <LedgerHead>
        <LedgerRow>
          <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
          <LedgerHeaderCell>{what}</LedgerHeaderCell>
          <LedgerHeaderCell numeric>Plays</LedgerHeaderCell>
          <LedgerHeaderCell numeric>Move</LedgerHeaderCell>
        </LedgerRow>
      </LedgerHead>
      <LedgerBody>
        {rows.map((row) => (
          <LedgerRow key={row.key}>
            <LedgerCell>
              <LedgerRank rank={row.rank} />
            </LedgerCell>
            <LedgerRowHeader>
              <Link
                to={row.to}
                className="block max-w-[12rem] truncate text-ink hover:text-lamp sm:max-w-[20rem]"
              >
                {row.name}
              </Link>
              {row.sub ? (
                <span className="block max-w-[12rem] truncate text-xs text-ink-muted sm:max-w-[20rem]">
                  {row.sub}
                </span>
              ) : null}
            </LedgerRowHeader>
            <LedgerCell numeric>{formatCount(row.plays)}</LedgerCell>
            <LedgerCell numeric>
              <Movement rank={row.rank} previousRank={row.previousRank} />
            </LedgerCell>
          </LedgerRow>
        ))}
      </LedgerBody>
    </Ledger>
  )
}

/** Rank movement against the preceding period. A null previous rank is new. */
function Movement({
  rank,
  previousRank,
}: {
  rank: number
  previousRank: number | null
}): ReactElement {
  const change = rankChange(rank, previousRank)
  if (change.direction === 'new') return <Chip tone="lamp">New</Chip>
  return (
    <>
      <span aria-hidden="true" className={change.direction === 'flat' ? 'text-ink-faint' : ''}>
        {change.label}
      </span>
      <span className="sr-only">{change.description}</span>
    </>
  )
}

/** How much of the range's listening time the leading artist accounts for. */
function LeaderShare({
  entry,
  total,
}: {
  entry: TopEntry<ArtistRef> | undefined
  total: number
}): ReactElement | null {
  if (!entry || total <= 0) return null
  return (
    <div className="border-t border-seam p-4">
      <ShareBar
        value={entry.msPlayed}
        total={total}
        label={`${entry.entity.name} — share of your listening time`}
        detail={`${formatDuration(entry.msPlayed)} of ${formatDuration(total)}`}
      />
    </div>
  )
}

/** The last few plays, newest first, as a strip that scrolls on its own. */
function RecentStrip({
  items,
  timeZone,
}: {
  items: HistoryItem[]
  timeZone: string
}): ReactElement {
  return (
    <ul className="flex snap-x gap-2 overflow-x-auto p-3">
      {items.map((item) => (
        <li
          key={item.id}
          className="w-52 shrink-0 snap-start rounded-control border border-seam p-3"
        >
          <RecentTitle track={item.track} alias={item.aliasTitle} />
          <p className="mt-0.5 truncate text-xs text-ink-muted">
            {artistsOf(item) || 'Unknown artist'}
          </p>
          <p className="mt-2 text-xs text-ink-faint">
            <time dateTime={item.playedAt}>{formatRelative(item.playedAt)}</time>
            <span className="sr-only"> — {formatDateTime(item.playedAt, timeZone)}</span>
          </p>
        </li>
      ))}
    </ul>
  )
}

function RecentTitle({
  track,
  alias,
}: {
  track: TrackRef | null
  alias: string | null
}): ReactElement {
  if (track) {
    return (
      <RangeLink
        to={`/tracks/${track.id}`}
        className="block truncate text-sm text-ink hover:text-lamp"
      >
        {track.name}
      </RangeLink>
    )
  }
  // A listen imported from an export can be names only until the catalogue
  // lookup resolves it; showing the name is honest, linking it would not be.
  return <p className="truncate text-sm text-ink">{alias ?? 'Unknown track'}</p>
}

function artistsOf(item: HistoryItem): string {
  if (item.track) return item.track.artists.map((artist) => artist.name).join(', ')
  return item.aliasArtist ?? ''
}
