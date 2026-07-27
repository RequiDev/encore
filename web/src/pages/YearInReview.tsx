/**
 * A year, told back to you.
 *
 * This is the one page in Encore that is allowed to raise its voice: the figures
 * are larger, the lamp is used more freely, and the top tens are given room. It
 * is still the same instrument — every number is a counter, every panel is flat —
 * but a retrospective that looked exactly like the dashboard would have no
 * reason to exist.
 *
 * The year selector only offers years the person actually has listening in,
 * derived from the first and last listen of their whole history. Offering 2008
 * to someone whose export starts in 2019 is eleven dead ends.
 */

import type { ReactElement } from 'react'
import { useMemo } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { calendarDayIn, presetRange } from '../lib/range'
import { useTimeZone } from '../lib/session'
import {
  EMPTY,
  formatClock,
  formatCount,
  formatDate,
  formatDateTime,
  formatDuration,
  formatPlural,
  formatTimeOfDay,
} from '../lib/format'
import type { Summary, YearInReview as YearInReviewData } from '../lib/types'
import {
  ButtonLink,
  EmptyState,
  ErrorState,
  Field,
  Icon,
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
  Select,
  Skeleton,
  SkeletonLedger,
  Stat,
  StatGrid,
} from '../components/ui'
import { BarChart, ChartCard } from '../components/charts'

/** Spotify launched in October 2008; the server refuses anything earlier. */
const EARLIEST_YEAR = 2008

const MINUTE_MS = 60_000

/**
 * `2026-07-26` as `26 Jul 2026`.
 *
 * A day key is a calendar date rather than an instant, so it is formatted in
 * UTC: rendering it in the reader's own zone would shift it to the day before
 * for anyone west of Greenwich.
 */
function dayLabel(day: string): string {
  return formatDate(`${day}T00:00:00Z`, 'UTC')
}

export default function YearInReview(): ReactElement {
  const { year: raw } = useParams<{ year: string }>()
  const timeZone = useTimeZone()
  const navigate = useNavigate()

  // A snapshot of "now", taken once rather than on every render: the answer only
  // changes at midnight, and a figure that can move mid-render is a bug source.
  const thisYear = useMemo(() => calendarDayIn(new Date(), timeZone).year, [timeZone])
  const parsed = Number(raw)
  const year = Number.isInteger(parsed) ? parsed : NaN
  const valid = Number.isInteger(year) && year >= EARLIEST_YEAR && year <= thisYear

  // The whole history, only for the first and last listen: it is what decides
  // which years the selector may offer.
  const everRange = useMemo(() => presetRange('all', timeZone), [timeZone])
  const ever = useQuery({
    queryKey: qk.summary(everRange),
    queryFn: ({ signal }) =>
      api.get<Summary>('/stats/summary', { from: everRange.from, to: everRange.to }, signal),
  })

  const years = useMemo(() => {
    const first = ever.data?.firstListenAt
    const last = ever.data?.lastListenAt
    if (!first || !last) return []
    const start = Math.max(calendarDayIn(new Date(first), timeZone).year, EARLIEST_YEAR)
    const end = Math.min(calendarDayIn(new Date(last), timeZone).year, thisYear)
    if (end < start) return []
    const all = Array.from({ length: end - start + 1 }, (_, index) => end - index)
    // A year reached by a hand-edited address still belongs in the list, or the
    // selector would silently disagree with the page it is on.
    return valid && !all.includes(year) ? [year, ...all].sort((a, b) => b - a) : all
  }, [ever.data, timeZone, thisYear, valid, year])

  const review = useQuery({
    queryKey: qk.yearInReview(valid ? year : 0),
    queryFn: ({ signal }) => api.get<YearInReviewData>('/stats/year-in-review', { year }, signal),
    enabled: valid,
  })

  const data = review.data ?? null
  const summary = data?.summary ?? null
  const minutes = Math.round((summary?.msPlayed ?? 0) / MINUTE_MS)
  const listens = summary?.listens ?? 0
  const daysInYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 366 : 365

  const status = !valid
    ? 'That year cannot be summarised.'
    : review.isPending
      ? `Loading your ${year} in review.`
      : review.isError
        ? `Your ${year} could not be loaded.`
        : listens === 0
          ? `Nothing was played in ${year}.`
          : `${year}: ${formatPlural(listens, 'listen')}, ${formatDuration(summary?.msPlayed ?? 0)}.`

  // If the history could not be read, the year on screen is still a real choice,
  // so the selector offers it rather than rendering an empty box.
  const options = years.length > 0 ? years : valid ? [year] : []

  const picker = (
    <Field label="Year" labelHidden className="w-32">
      <Select
        value={valid ? String(year) : ''}
        disabled={ever.isPending || options.length === 0}
        onChange={(event) => {
          void navigate(`/year/${event.target.value}`)
        }}
      >
        {valid ? null : <option value="">Choose a year</option>}
        {options.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </Select>
    </Field>
  )

  return (
    <div className="space-y-4">
      <PageHeader
        title={valid ? `${year} in review` : 'Year in review'}
        description={
          valid
            ? `Everything you played between 1 January and 31 December ${year}, in your timezone.`
            : 'A year of listening, summarised.'
        }
        actions={picker}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {!valid ? (
        <Panel padded={false}>
          <EmptyState
            icon="year"
            title="That is not a year Encore can summarise"
            description={`Pick a year between ${EARLIEST_YEAR} and ${thisYear} — Spotify did not exist before that, and next year has not happened yet.`}
            action={
              <ButtonLink to={`/year/${years[0] ?? thisYear}`} variant="primary">
                Show {years[0] ?? thisYear}
              </ButtonLink>
            }
          />
        </Panel>
      ) : ever.isSuccess && years.length === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon="import"
            title="Nothing to look back on yet"
            description="Encore has no listening history for this account, so there is no year to summarise. Import a Spotify export and this page fills itself in."
            action={
              <ButtonLink to="/imports" variant="primary">
                Import your history
              </ButtonLink>
            }
          />
        </Panel>
      ) : review.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={review.error}
            title={`Your ${year} could not be loaded`}
            onRetry={() => {
              void review.refetch()
            }}
          />
        </Panel>
      ) : review.isSuccess && listens === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon="year"
            title={`Nothing played in ${year}`}
            description={
              years.length > 0
                ? `Encore holds listening for ${years[years.length - 1]} to ${years[0]}. Pick one of those years above.`
                : 'Import a Spotify export and this page fills itself in.'
            }
            action={
              years[0] !== undefined && years[0] !== year ? (
                <ButtonLink to={`/year/${years[0]}`} variant="primary">
                  Show {years[0]}
                </ButtonLink>
              ) : (
                <ButtonLink to="/imports" variant="primary">
                  Import your history
                </ButtonLink>
              )
            }
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={4}>
            <Stat
              label="Minutes listened"
              value={formatCount(minutes)}
              lamp
              loading={review.isPending}
              hint={
                review.isPending ? 'Adding up the year' : formatDuration(summary?.msPlayed ?? 0)
              }
            />
            <Stat
              label="Listens"
              value={formatCount(listens)}
              loading={review.isPending}
              hint={
                review.isPending
                  ? 'Counting the year'
                  : `About ${formatCount(Math.round(listens / daysInYear))} a day`
              }
            />
            <Stat
              label="Artists discovered"
              value={formatCount(data?.newArtists ?? 0)}
              loading={review.isPending}
              hint="Heard for the first time ever this year"
            />
            <Stat
              label="Active days"
              value={formatCount(summary?.activeDays ?? 0)}
              suffix={`of ${daysInYear}`}
              meter={(summary?.activeDays ?? 0) / daysInYear}
              loading={review.isPending}
              hint="Days with at least one listen"
            />
          </StatGrid>

          <StatGrid columns={3}>
            <Stat
              label="Distinct tracks"
              value={formatCount(summary?.distinctTracks ?? 0)}
              loading={review.isPending}
            />
            <Stat
              label="Distinct artists"
              value={formatCount(summary?.distinctArtists ?? 0)}
              loading={review.isPending}
            />
            <Stat
              label="Distinct albums"
              value={formatCount(summary?.distinctAlbums ?? 0)}
              loading={review.isPending}
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <Panel title="Busiest day" description={`The single heaviest day of ${year}`}>
              {review.isPending ? (
                <Skeleton className="h-20 w-full" />
              ) : data?.busiestDay ? (
                <div>
                  <p className="counter counter-lamp">{formatCount(data.busiestDay.plays)}</p>
                  <p className="mt-1 text-sm text-ink-muted">
                    {data.busiestDay.plays === 1 ? 'listen on ' : 'listens on '}
                    <span className="text-ink">{dayLabel(data.busiestDay.day)}</span>
                  </p>
                  <p className="mt-3 text-sm text-ink-muted">
                    <span className="tabular text-ink">
                      {formatDuration(data.busiestDay.msPlayed)}
                    </span>{' '}
                    of listening that day
                  </p>
                </div>
              ) : (
                <p className="text-sm text-ink-muted">No day in {year} carried a listen.</p>
              )}
            </Panel>

            <Panel
              title="Longest session"
              description="The longest unbroken run, gaps under 30 minutes"
            >
              {review.isPending ? (
                <Skeleton className="h-20 w-full" />
              ) : data?.longestSession ? (
                <LongestSession session={data.longestSession} timeZone={timeZone} />
              ) : (
                <p className="text-sm text-ink-muted">
                  No listening session was recorded in {year}.
                </p>
              )}
            </Panel>
          </div>

          <ChartCard
            title="The year's artists"
            description="Your ten most played, by number of listens"
          >
            {review.isPending ? (
              <div role="status" aria-busy="true" className="p-1" style={{ height: 340 }}>
                <span className="sr-only">Loading the year&apos;s top artists</span>
                <Skeleton className="h-full w-full" />
              </div>
            ) : (
              <BarChart
                data={(data?.topArtists ?? []).map((entry) => ({
                  key: entry.entity.id,
                  label: entry.entity.name,
                  value: entry.plays,
                }))}
                label={`Most played artists of ${year}`}
                valueName="plays"
                busy={review.isFetching}
                emptyDescription={`Nothing was played in ${year}, so there is nothing to rank.`}
              />
            )}
          </ChartCard>

          <div className="grid gap-4 xl:grid-cols-2">
            <TopPanel
              title="Top tracks"
              what="Track"
              loading={review.isPending}
              rows={(data?.topTracks ?? []).map((entry) => ({
                key: entry.entity.id,
                to: `/tracks/${entry.entity.id}`,
                name: entry.entity.name,
                sub: entry.entity.artists.map((artist) => artist.name).join(', '),
                plays: entry.plays,
                msPlayed: entry.msPlayed,
                rank: entry.rank,
              }))}
            />
            <TopPanel
              title="Top artists"
              what="Artist"
              loading={review.isPending}
              rows={(data?.topArtists ?? []).map((entry) => ({
                key: entry.entity.id,
                to: `/artists/${entry.entity.id}`,
                name: entry.entity.name,
                plays: entry.plays,
                msPlayed: entry.msPlayed,
                rank: entry.rank,
              }))}
            />
          </div>

          <TopPanel
            title="Top albums"
            what="Album"
            loading={review.isPending}
            rows={(data?.topAlbums ?? []).map((entry) => ({
              key: entry.entity.id,
              to: `/albums/${entry.entity.id}`,
              name: entry.entity.name,
              plays: entry.plays,
              msPlayed: entry.msPlayed,
              rank: entry.rank,
            }))}
          />

          <p className="text-xs text-ink-faint">
            First listen of the year{' '}
            <span className="tabular">
              {summary?.firstListenAt ? formatDateTime(summary.firstListenAt, timeZone) : EMPTY}
            </span>
            , last{' '}
            <span className="tabular">
              {summary?.lastListenAt ? formatDateTime(summary.lastListenAt, timeZone) : EMPTY}
            </span>
            .
          </p>
        </>
      )}
    </div>
  )
}

/** The year's longest run, with the tracks it was made of behind a disclosure. */
function LongestSession({
  session,
  timeZone,
}: {
  session: NonNullable<YearInReviewData['longestSession']>
  timeZone: string
}): ReactElement {
  return (
    <div>
      <p className="counter">{formatDuration(session.msPlayed)}</p>
      <p className="mt-1 text-sm text-ink-muted">
        <time dateTime={session.startedAt} className="tabular text-ink">
          {formatDateTime(session.startedAt, timeZone)}
        </time>{' '}
        to <span className="tabular text-ink">{formatTimeOfDay(session.endedAt, timeZone)}</span>,{' '}
        {formatPlural(session.trackCount, 'track')}
      </p>

      {session.tracks.length > 0 ? (
        <details className="group mt-3">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 text-sm text-ink-muted hover:text-lamp [&::-webkit-details-marker]:hidden">
            <span aria-hidden="true" className="transition-transform group-open:rotate-180">
              <Icon name="chevron-down" />
            </span>
            What played
          </summary>
          <ol className="mt-2 space-y-1.5">
            {session.tracks.map((track, index) => (
              <li key={`${track.id}-${index}`} className="flex items-baseline gap-3 text-sm">
                <span className="tabular w-6 shrink-0 text-right text-ink-faint">{index + 1}</span>
                <span className="min-w-0 flex-1">
                  <RangeLink to={`/tracks/${track.id}`} className="text-ink hover:text-lamp">
                    {track.name}
                  </RangeLink>
                  <span className="block truncate text-xs text-ink-muted">
                    {track.artists.map((artist) => artist.name).join(', ') || 'Unknown artist'}
                  </span>
                </span>
                <span className="tabular shrink-0 text-xs text-ink-faint">
                  {formatClock(track.durationMs)}
                </span>
              </li>
            ))}
          </ol>
        </details>
      ) : (
        <p className="mt-3 text-sm text-ink-faint">
          The tracks in this session are still being fetched from Spotify.
        </p>
      )}
    </div>
  )
}

interface TopRow {
  key: string
  to: string
  name: string
  sub?: string
  plays: number
  msPlayed: number
  rank: number
}

/** One of the year's three top tens. */
function TopPanel({
  title,
  what,
  rows,
  loading,
}: {
  title: string
  what: string
  rows: TopRow[]
  loading: boolean
}): ReactElement {
  return (
    <Panel title={title} description="Ranked by plays across the year" padded={false}>
      {loading ? (
        <SkeletonLedger rows={10} columns={4} />
      ) : rows.length === 0 ? (
        <EmptyState
          title={`No ${what.toLowerCase()}s to rank`}
          description="Nothing with a resolved catalogue entry was played this year."
        />
      ) : (
        <Ledger caption={`${title} of the year, ranked by plays`}>
          <LedgerHead>
            <LedgerRow>
              <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
              <LedgerHeaderCell>{what}</LedgerHeaderCell>
              <LedgerHeaderCell numeric>Plays</LedgerHeaderCell>
              <LedgerHeaderCell numeric>Time</LedgerHeaderCell>
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
                    className="block max-w-[13rem] truncate text-ink hover:text-lamp sm:max-w-[20rem]"
                  >
                    {row.name}
                  </Link>
                  {row.sub ? (
                    <span className="block max-w-[13rem] truncate text-xs text-ink-muted sm:max-w-[20rem]">
                      {row.sub}
                    </span>
                  ) : null}
                </LedgerRowHeader>
                <LedgerCell numeric>{formatCount(row.plays)}</LedgerCell>
                <LedgerCell numeric className="whitespace-nowrap">
                  {formatDuration(row.msPlayed)}
                </LedgerCell>
              </LedgerRow>
            ))}
          </LedgerBody>
        </Ledger>
      )}
    </Panel>
  )
}
