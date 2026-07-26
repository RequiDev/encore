/**
 * Compare: two periods of your own listening, or your listening against someone
 * else's on this instance.
 *
 * Both live on one route because they answer the same question with different
 * second operands, and a single picker at the top chooses which. That keeps one
 * h1 and one range control on the page rather than two near-identical screens.
 *
 * Two decisions are worth recording. The later period is the shared URL range —
 * the same one every other page uses — so arriving here from the dashboard
 * compares what you were already looking at; only the *earlier* period gets its
 * own control, and it defaults to the equal-length window immediately before.
 * And the deltas are never coloured green and red: listening more is not success
 * and listening less is not failure, so the sign and a word carry the direction
 * and nothing else does.
 */

import type { ReactElement, ReactNode } from 'react'
import { useMemo, useState } from 'react'
import { Link, useLocation, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import {
  addDays,
  calendarDayIn,
  parseDayInput,
  startOfDayIn,
  toDayInput,
  useRange,
} from '../lib/range'
import type { DateRange } from '../lib/range'
import {
  formatCount,
  formatDate,
  formatDuration,
  formatPercent,
  formatPlural,
  formatSigned,
} from '../lib/format'
import type { AffinityResponse, CompareResponse, PublicUser } from '../lib/types'
import {
  Button,
  ButtonLink,
  EmptyState,
  ErrorState,
  Field,
  Input,
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
  Skeleton,
  SkeletonLedger,
  Select,
  Stat,
  StatGrid,
} from '../components/ui'
import { ShareBar } from '../components/charts'

const DAY_MS = 86_400_000

/** `+4h 12m`, the duration equivalent of `formatSigned`. */
function signedDuration(value: number): string {
  if (!Number.isFinite(value) || value === 0) return formatDuration(0)
  return `${value > 0 ? '+' : '-'}${formatDuration(Math.abs(value))}`
}

/**
 * The change as a percentage of the earlier period, or null when there is no
 * earlier figure to be a percentage of — a rise from nothing is "new", not
 * "+∞%".
 */
function changePercent(delta: number, base: number): string | null {
  if (!Number.isFinite(delta) || !Number.isFinite(base) || base <= 0) return null
  return `${delta > 0 ? '+' : ''}${formatPercent(delta / base)}`
}

function lengthInDays(range: DateRange): number {
  const ms = Date.parse(range.to) - Date.parse(range.from)
  if (!Number.isFinite(ms)) return 0
  return Math.max(Math.round(ms / DAY_MS), 1)
}

/** The equal-length window immediately before this one. */
function precedingRange(range: DateRange): DateRange {
  const from = Date.parse(range.from)
  const to = Date.parse(range.to)
  if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) return range
  return { from: new Date(from - (to - from)).toISOString(), to: range.from }
}

/** `1 Jan 2026 to 30 Jan 2026`, with the exclusive end shown as the day people mean. */
function rangeDays(range: DateRange, timeZone: string): string {
  const first = formatDate(range.from, timeZone)
  const last = formatDate(new Date(Date.parse(range.to) - 1), timeZone)
  return `${first} to ${last}`
}

export default function Compare(): ReactElement {
  const { userId } = useParams<{ userId?: string }>()
  const { label } = useRange()

  return (
    <div className="space-y-4">
      <PageHeader
        title="Compare"
        description={
          userId
            ? `Your listening against someone else's — ${label.toLowerCase()}.`
            : `Two periods of your listening, side by side. The later one is ${label.toLowerCase()}.`
        }
        actions={<RangePicker />}
      />

      <TargetPicker userId={userId ?? null} />

      {userId ? <PeopleComparison userId={userId} /> : <PeriodComparison />}
    </div>
  )
}

// --- choosing what to compare against --------------------------------------

/** The one control that switches between the two comparisons. */
function TargetPicker({ userId }: { userId: string | null }): ReactElement {
  const navigate = useNavigate()
  const { search } = useLocation()

  const users = useQuery({
    queryKey: qk.users(),
    queryFn: ({ signal }) => api.get<PublicUser[]>('/users', undefined, signal),
  })

  const people = users.data ?? []

  return (
    <Panel
      title="Compare with"
      description="Two periods of your own listening, or another account on this instance."
    >
      {users.isPending ? (
        <Skeleton className="h-9 w-64" />
      ) : users.isError ? (
        <div className="flex flex-wrap items-center gap-3">
          <p className="text-sm text-ink-muted">
            The list of people could not be loaded. You can still compare two periods.
          </p>
          <Button
            size="sm"
            onClick={() => {
              void users.refetch()
            }}
          >
            Try again
          </Button>
          {userId ? <ButtonLink to={`/compare${search}`}>Compare two periods</ButtonLink> : null}
        </div>
      ) : people.length === 0 ? (
        <p className="text-sm text-ink-muted">
          You are the only account on this instance, so there is nobody to compare with. Two periods
          of your own listening are compared below.
        </p>
      ) : (
        <Field
          label="Compare with"
          labelHidden
          hint="Only accounts on this instance appear here, and only shared artists, albums and tracks are exchanged."
          className="max-w-sm"
        >
          <Select
            value={userId ?? 'periods'}
            onChange={(event) => {
              const value = event.target.value
              void navigate(
                value === 'periods' ? `/compare${search}` : `/compare/${value}${search}`,
              )
            }}
          >
            <option value="periods">Two periods of my own listening</option>
            {people.map((person) => (
              <option key={person.id} value={person.id}>
                {person.displayName}
              </option>
            ))}
          </Select>
        </Field>
      )}
    </Panel>
  )
}

// --- period against period --------------------------------------------------

function PeriodComparison(): ReactElement {
  const { range, timeZone, label } = useRange()
  const [params, setParams] = useSearchParams()

  const aFrom = params.get('aFrom')
  const aTo = params.get('aTo')

  /** The earlier period: whatever the URL carries, or the preceding window. */
  const earlier = useMemo<DateRange>(() => {
    if (aFrom && aTo && Date.parse(aFrom) < Date.parse(aTo)) {
      return { from: new Date(aFrom).toISOString(), to: new Date(aTo).toISOString() }
    }
    return precedingRange(range)
  }, [aFrom, aTo, range])

  const setEarlier = (next: DateRange | null): void => {
    setParams(
      (current) => {
        const updated = new URLSearchParams(current)
        if (next) {
          updated.set('aFrom', next.from)
          updated.set('aTo', next.to)
        } else {
          updated.delete('aFrom')
          updated.delete('aTo')
        }
        return updated
      },
      { replace: true },
    )
  }

  const compare = useQuery({
    queryKey: qk.compare(earlier, range),
    queryFn: ({ signal }) =>
      api.get<CompareResponse>(
        '/stats/compare',
        { aFrom: earlier.from, aTo: earlier.to, bFrom: range.from, bTo: range.to },
        signal,
      ),
  })

  const a = compare.data?.a.summary ?? null
  const b = compare.data?.b.summary ?? null
  const delta = compare.data?.delta ?? null

  const earlierDays = lengthInDays(earlier)
  const laterDays = lengthInDays(range)
  const mismatched = earlierDays !== laterDays

  const status = compare.isPending
    ? 'Comparing the two periods.'
    : compare.isError
      ? 'The comparison could not be loaded.'
      : `${formatPlural(b?.listens ?? 0, 'listen')} in the later period against ${formatPlural(a?.listens ?? 0, 'listen')} in the earlier one.`

  return (
    <>
      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <Panel
        title="Earlier period"
        description="The baseline everything below is measured against. It starts as the equal-length window immediately before the range at the top of the page."
      >
        <EarlierPeriodForm
          earlier={earlier}
          timeZone={timeZone}
          isDefault={!aFrom || !aTo}
          onApply={setEarlier}
          onReset={() => setEarlier(null)}
        />
      </Panel>

      {compare.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={compare.error}
            title="The comparison could not be loaded"
            onRetry={() => {
              void compare.refetch()
            }}
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={3}>
            <Stat
              label="Listens"
              value={formatCount(b?.listens ?? 0)}
              lamp
              loading={compare.isPending}
              hint={
                <Delta
                  change={delta?.listens ?? null}
                  base={a?.listens ?? 0}
                  format={formatSigned}
                />
              }
            />
            <Stat
              label="Listening time"
              value={formatDuration(b?.msPlayed ?? 0)}
              loading={compare.isPending}
              hint={
                <Delta
                  change={delta?.msPlayed ?? null}
                  base={a?.msPlayed ?? 0}
                  format={signedDuration}
                />
              }
            />
            <Stat
              label="Active days"
              value={formatCount(b?.activeDays ?? 0)}
              suffix={`of ${formatCount(laterDays)}`}
              meter={(b?.activeDays ?? 0) / Math.max(laterDays, 1)}
              loading={compare.isPending}
              hint={
                <Delta
                  change={a && b ? b.activeDays - a.activeDays : null}
                  base={a?.activeDays ?? 0}
                  format={formatSigned}
                />
              }
            />
          </StatGrid>

          <Panel
            title="Every figure"
            description={
              mismatched
                ? `The periods are different lengths — ${formatPlural(earlierDays, 'day')} against ${formatPlural(laterDays, 'day')} — so the change is not like for like.`
                : `Both periods are ${formatPlural(laterDays, 'day')} long.`
            }
            padded={false}
          >
            {compare.isPending ? (
              <SkeletonLedger rows={6} columns={5} />
            ) : (
              <Ledger caption="Each figure in the earlier period, the later period, and the change between them">
                <LedgerHead>
                  <LedgerRow>
                    <LedgerHeaderCell>Figure</LedgerHeaderCell>
                    <LedgerHeaderCell numeric>
                      Earlier
                      <span className="block font-normal normal-case">
                        {rangeDays(earlier, timeZone)}
                      </span>
                    </LedgerHeaderCell>
                    <LedgerHeaderCell numeric>
                      Later
                      <span className="block font-normal normal-case">
                        {rangeDays(range, timeZone)}
                      </span>
                    </LedgerHeaderCell>
                    <LedgerHeaderCell numeric>Change</LedgerHeaderCell>
                    <LedgerHeaderCell numeric>Change %</LedgerHeaderCell>
                  </LedgerRow>
                </LedgerHead>
                <LedgerBody>
                  <FigureRow
                    name="Listens"
                    a={a?.listens ?? 0}
                    b={b?.listens ?? 0}
                    change={delta?.listens ?? 0}
                    format={formatCount}
                    formatChange={formatSigned}
                  />
                  <FigureRow
                    name="Listening time"
                    a={a?.msPlayed ?? 0}
                    b={b?.msPlayed ?? 0}
                    change={delta?.msPlayed ?? 0}
                    format={formatDuration}
                    formatChange={signedDuration}
                  />
                  <FigureRow
                    name="Active days"
                    a={a?.activeDays ?? 0}
                    b={b?.activeDays ?? 0}
                    change={(b?.activeDays ?? 0) - (a?.activeDays ?? 0)}
                    format={formatCount}
                    formatChange={formatSigned}
                  />
                  <FigureRow
                    name="Distinct tracks"
                    a={a?.distinctTracks ?? 0}
                    b={b?.distinctTracks ?? 0}
                    change={delta?.distinctTracks ?? 0}
                    format={formatCount}
                    formatChange={formatSigned}
                  />
                  <FigureRow
                    name="Distinct artists"
                    a={a?.distinctArtists ?? 0}
                    b={b?.distinctArtists ?? 0}
                    change={delta?.distinctArtists ?? 0}
                    format={formatCount}
                    formatChange={formatSigned}
                  />
                  <FigureRow
                    name="Distinct albums"
                    a={a?.distinctAlbums ?? 0}
                    b={b?.distinctAlbums ?? 0}
                    change={delta?.distinctAlbums ?? 0}
                    format={formatCount}
                    formatChange={formatSigned}
                  />
                </LedgerBody>
              </Ledger>
            )}
          </Panel>

          {compare.isSuccess && (a?.listens ?? 0) === 0 && (b?.listens ?? 0) === 0 ? (
            <Panel padded={false}>
              <EmptyState
                icon="compare"
                title="Neither period has any listens"
                description={`Nothing was played in ${label.toLowerCase()} or in the period before it. Widen the range at the top of the page, or import more of your history.`}
                action={
                  <ButtonLink to="/imports" variant="primary">
                    Import your history
                  </ButtonLink>
                }
              />
            </Panel>
          ) : null}
        </>
      )}
    </>
  )
}

/** One measure across both periods. */
function FigureRow({
  name,
  a,
  b,
  change,
  format,
  formatChange,
}: {
  name: string
  a: number
  b: number
  change: number
  format: (value: number) => string
  formatChange: (value: number) => string
}): ReactElement {
  const percent = changePercent(change, a)
  return (
    <LedgerRow>
      <LedgerRowHeader>{name}</LedgerRowHeader>
      <LedgerCell numeric>{format(a)}</LedgerCell>
      <LedgerCell numeric>{format(b)}</LedgerCell>
      <LedgerCell numeric>
        {change === 0 ? <span className="text-ink-faint">no change</span> : formatChange(change)}
        {change !== 0 ? (
          <span className="sr-only">
            {' '}
            {change > 0 ? 'more than' : 'less than'} the earlier period
          </span>
        ) : null}
      </LedgerCell>
      <LedgerCell numeric>
        {percent ?? <span className="text-ink-faint">new</span>}
        {percent ? null : <span className="sr-only">No earlier figure to compare with</span>}
      </LedgerCell>
    </LedgerRow>
  )
}

/** A change against the earlier period, in words as well as in sign. */
function Delta({
  change,
  base,
  format,
}: {
  change: number | null
  base: number
  format: (value: number) => string
}): ReactElement {
  if (change === null) return <span>Comparing with the earlier period…</span>
  if (change === 0) return <span>No change on the earlier period</span>
  const percent = changePercent(change, base)
  return (
    <span className="inline-flex flex-wrap items-baseline gap-1">
      <span className="tabular text-ink-muted">{format(change)}</span>
      {percent ? <span className="tabular text-ink-faint">({percent})</span> : null}
      <span className="sr-only">{change > 0 ? 'more' : 'fewer'}</span>
      <span>on the earlier period</span>
    </span>
  )
}

/**
 * The baseline's own dates.
 *
 * Two real date inputs, so the browser's calendar and its locale do the work,
 * and both days are inclusive — the exclusive end the API wants is arithmetic
 * nobody should have to do in their head.
 */
function EarlierPeriodForm({
  earlier,
  timeZone,
  isDefault,
  onApply,
  onReset,
}: {
  earlier: DateRange
  timeZone: string
  isDefault: boolean
  onApply: (range: DateRange) => void
  onReset: () => void
}): ReactElement {
  const days = useMemo(
    () => ({
      from: toDayInput(calendarDayIn(new Date(earlier.from), timeZone)),
      to: toDayInput(calendarDayIn(new Date(Date.parse(earlier.to) - 1), timeZone)),
    }),
    [earlier, timeZone],
  )

  const [start, setStart] = useState(days.from)
  const [end, setEnd] = useState(days.to)
  // The inputs follow the URL when it changes underneath them — which happens
  // when the range at the top of the page moves the default baseline.
  const [seen, setSeen] = useState(days)
  if (seen.from !== days.from || seen.to !== days.to) {
    setSeen(days)
    setStart(days.from)
    setEnd(days.to)
  }

  const invalid = start === '' || end === '' || start > end

  return (
    <form
      className="flex flex-wrap items-end gap-3"
      onSubmit={(event) => {
        event.preventDefault()
        const first = parseDayInput(start)
        const last = parseDayInput(end)
        if (!first || !last) return
        const from = startOfDayIn(first, timeZone)
        // The picker's end day is inclusive; the API's `to` is not.
        const to = startOfDayIn(addDays(last, 1), timeZone)
        if (to.getTime() <= from.getTime()) return
        onApply({ from: from.toISOString(), to: to.toISOString() })
      }}
    >
      <Field label="From" className="w-40">
        <Input type="date" value={start} max={end} onChange={(e) => setStart(e.target.value)} />
      </Field>
      <Field
        label="To"
        className="w-40"
        error={invalid && start !== '' && end !== '' ? 'The first day must not be later.' : null}
      >
        <Input type="date" value={end} min={start} onChange={(e) => setEnd(e.target.value)} />
      </Field>
      <Button type="submit" variant="primary" disabled={invalid}>
        Apply
      </Button>
      <Button onClick={onReset} disabled={isDefault}>
        Use the preceding period
      </Button>
    </form>
  )
}

// --- person against person --------------------------------------------------

function PeopleComparison({ userId }: { userId: string }): ReactElement {
  const { range, label } = useRange()

  const affinity = useQuery({
    queryKey: qk.affinity(userId, range),
    queryFn: ({ signal }) =>
      api.get<AffinityResponse>(
        `/stats/affinity/${userId}`,
        { from: range.from, to: range.to },
        signal,
      ),
  })

  const data = affinity.data ?? null
  const them = data?.user.displayName ?? 'They'
  const shared =
    (data?.artists.length ?? 0) + (data?.albums.length ?? 0) + (data?.tracks.length ?? 0)

  const status = affinity.isPending
    ? 'Loading the comparison.'
    : affinity.isError
      ? 'The comparison could not be loaded.'
      : `You and ${them} match ${formatPercent(data?.score ?? 0)} in ${label.toLowerCase()}.`

  if (affinity.isError) {
    return (
      <>
        <p role="status" aria-live="polite" className="sr-only">
          {status}
        </p>
        <Panel padded={false}>
          <ErrorState
            error={affinity.error}
            title="That comparison could not be loaded"
            onRetry={() => {
              void affinity.refetch()
            }}
          >
            <Link to="/compare" className="text-sm text-ink-muted hover:text-lamp">
              Compare two periods instead
            </Link>
          </ErrorState>
        </Panel>
      </>
    )
  }

  return (
    <>
      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <Panel
        title="How close you are"
        description="Similarity compares the shape of your artist listening, not its size — someone who listens far more than you can still match you perfectly."
      >
        <div className="flex flex-wrap items-center gap-x-8 gap-y-4">
          <div className="flex items-center gap-3">
            {data?.user.avatarUrl ? (
              <img
                src={data.user.avatarUrl}
                alt=""
                className="h-10 w-10 rounded-full border border-seam object-cover"
              />
            ) : null}
            <div className="min-w-0">
              <p className="eyebrow">You and</p>
              <p className="truncate text-sm font-medium text-ink">
                {affinity.isPending ? '…' : them}
              </p>
            </div>
          </div>

          <div className="min-w-[12rem] flex-1">
            {affinity.isPending ? (
              <Skeleton className="h-10 w-full" />
            ) : (
              <ShareBar
                value={data?.score ?? 0}
                total={1}
                label={`Similarity with ${them}, ${label.toLowerCase()}`}
                detail={`0% means nothing in common, 100% means identical proportions. Based on ${formatPlural(data?.artists.length ?? 0, 'shared artist')}.`}
              />
            )}
          </div>
        </div>
      </Panel>

      <StatGrid columns={3}>
        <Stat
          label="Shared artists"
          value={formatCount(data?.artists.length ?? 0)}
          loading={affinity.isPending}
          hint="Both of you played them in this range"
        />
        <Stat
          label="Shared albums"
          value={formatCount(data?.albums.length ?? 0)}
          loading={affinity.isPending}
          hint="Both of you played them in this range"
        />
        <Stat
          label="Shared tracks"
          value={formatCount(data?.tracks.length ?? 0)}
          loading={affinity.isPending}
          hint="Both of you played them in this range"
        />
      </StatGrid>

      {affinity.isSuccess && shared === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon="compare"
            title={`Nothing in common with ${them} in this range`}
            description="Neither of you played the same artist between these dates. A wider range usually finds the overlap."
          />
        </Panel>
      ) : (
        <>
          <div className="grid gap-4 lg:grid-cols-2">
            <SharedPanel
              title="Shared artists"
              them={them}
              loading={affinity.isPending}
              rows={(data?.artists ?? []).map((entry) => ({
                key: entry.entity.id,
                name: entry.entity.name,
                to: `/artists/${entry.entity.id}`,
                playsA: entry.playsA,
                playsB: entry.playsB,
              }))}
            />
            <SharedPanel
              title="Shared albums"
              them={them}
              loading={affinity.isPending}
              rows={(data?.albums ?? []).map((entry) => ({
                key: entry.entity.id,
                name: entry.entity.name,
                to: `/albums/${entry.entity.id}`,
                playsA: entry.playsA,
                playsB: entry.playsB,
              }))}
            />
          </div>

          <SharedPanel
            title="Shared tracks"
            them={them}
            loading={affinity.isPending}
            rows={(data?.tracks ?? []).map((entry) => ({
              key: entry.entity.id,
              name: entry.entity.name,
              sub: entry.entity.artists.map((artist) => artist.name).join(', '),
              to: `/tracks/${entry.entity.id}`,
              playsA: entry.playsA,
              playsB: entry.playsB,
            }))}
          />
        </>
      )}
    </>
  )
}

interface SharedRow {
  key: string
  name: string
  sub?: string
  to: string
  playsA: number
  playsB: number
}

/** One of the three overlap tables: what you both played, and how much each. */
function SharedPanel({
  title,
  them,
  rows,
  loading,
}: {
  title: string
  them: string
  rows: SharedRow[]
  loading: boolean
}): ReactElement {
  let body: ReactNode
  if (loading) {
    body = <SkeletonLedger rows={6} columns={3} />
  } else if (rows.length === 0) {
    body = (
      <EmptyState
        title="Nothing in common here"
        description="Neither of you played the same one in this range."
      />
    )
  } else {
    body = (
      <Ledger caption={`${title}, with each side's play counts`}>
        <LedgerHead>
          <LedgerRow>
            <LedgerHeaderCell>Name</LedgerHeaderCell>
            <LedgerHeaderCell numeric>You</LedgerHeaderCell>
            <LedgerHeaderCell numeric>{them}</LedgerHeaderCell>
          </LedgerRow>
        </LedgerHead>
        <LedgerBody>
          {rows.map((row) => (
            <LedgerRow key={row.key}>
              <LedgerRowHeader>
                <Link
                  to={row.to}
                  className="block max-w-[14rem] truncate text-ink hover:text-lamp sm:max-w-[22rem]"
                >
                  {row.name}
                </Link>
                {row.sub ? (
                  <span className="block max-w-[14rem] truncate text-xs text-ink-muted sm:max-w-[22rem]">
                    {row.sub}
                  </span>
                ) : null}
              </LedgerRowHeader>
              <LedgerCell numeric>{formatCount(row.playsA)}</LedgerCell>
              <LedgerCell numeric>{formatCount(row.playsB)}</LedgerCell>
            </LedgerRow>
          ))}
        </LedgerBody>
      </Ledger>
    )
  }

  return (
    <Panel title={title} description="Most played together first" padded={false}>
      {body}
    </Panel>
  )
}
