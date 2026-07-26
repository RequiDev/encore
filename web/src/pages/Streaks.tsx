/**
 * Streaks: runs of consecutive days with something played on them.
 *
 * This is the one statistics page with no date range, because the endpoint has
 * none: a streak is a fact about a whole listening history, and scoping it to a
 * window would cut runs in half at the window's edges and report the pieces as
 * if they were the whole. The page says that rather than quietly omitting the
 * control everything else has.
 *
 * The rule for what counts as a day is stated in full on the page. It has two
 * halves people get wrong — the day is a local calendar day, not 24 hours from
 * the last listen, and a streak is not broken until a whole day passes with
 * nothing on it, so "nothing yet today" is not a broken streak at breakfast.
 */

import type { ReactElement } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { ALL_TIME_START } from '../lib/range'
import type { DateRange } from '../lib/range'
import { useTimeZone } from '../lib/session'
import { formatCount, formatDate, formatPlural } from '../lib/format'
import type { Streak, StreaksResponse } from '../lib/types'
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
  SkeletonLedger,
  Stat,
  StatGrid,
} from '../components/ui'

/**
 * Streaks take no range, but the shared key helper wants one. A single fixed
 * value keeps every streak query on one cache entry, still under the `stats`
 * prefix so that finishing an import invalidates it with everything else. It is
 * never sent to the server.
 */
const WHOLE_HISTORY: DateRange = { from: ALL_TIME_START, to: ALL_TIME_START }

/** How many day cells a band draws before it says "and N earlier days". */
const BAND_DAYS = 30

const DAY_MS = 86_400_000

/**
 * `2026-07-26` as `26 Jul 2026`.
 *
 * A day key is a calendar date, not an instant, so it is formatted in UTC —
 * rendering it in the user's zone would shift it to the day before for anyone
 * west of Greenwich, which is exactly the bug this exists to avoid.
 */
function dayLabel(day: string): string {
  return formatDate(`${day}T00:00:00Z`, 'UTC')
}

/** The day keys a streak covers, most recent last, capped at `limit`. */
function daysOf(streak: Streak, limit: number): string[] {
  const end = Date.parse(`${streak.endDay}T00:00:00Z`)
  if (!Number.isFinite(end)) return []
  const shown = Math.min(Math.max(streak.days, 1), limit)
  return Array.from({ length: shown }, (_, index) =>
    new Date(end - (shown - 1 - index) * DAY_MS).toISOString().slice(0, 10),
  )
}

function sameStreak(a: Streak | null, b: Streak | null): boolean {
  return a !== null && b !== null && a.startDay === b.startDay && a.endDay === b.endDay
}

export default function Streaks(): ReactElement {
  const timeZone = useTimeZone()

  const streaks = useQuery({
    queryKey: qk.streaks(WHOLE_HISTORY),
    queryFn: ({ signal }) => api.get<StreaksResponse>('/stats/streaks', undefined, signal),
  })

  const current = streaks.data?.current ?? null
  const longest = streaks.data?.longest ?? null
  const top = streaks.data?.top ?? []
  const nothing = streaks.isSuccess && current === null && longest === null && top.length === 0

  const status = streaks.isPending
    ? 'Loading your listening streaks.'
    : streaks.isError
      ? 'Your listening streaks could not be loaded.'
      : nothing
        ? 'No listening streaks yet.'
        : current
          ? `Current streak ${formatPlural(current.days, 'day')}. Longest ever ${formatPlural(longest?.days ?? 0, 'day')}.`
          : `No streak is running. Longest ever ${formatPlural(longest?.days ?? 0, 'day')}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title="Streaks"
        description="Runs of consecutive days with at least one listen, over your whole history."
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <p className="max-w-prose text-sm text-ink-muted">
        A day counts when you played at least one thing on it — one listen is enough — measured from
        midnight to midnight in your timezone, {timeZone}. A streak stays alive until a whole day
        passes with nothing on it, so having played nothing yet today does not break it. Dates are
        not filtered here: streaks are counted over everything Encore holds.
      </p>

      {streaks.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={streaks.error}
            title="Your listening streaks could not be loaded"
            onRetry={() => {
              void streaks.refetch()
            }}
          />
        </Panel>
      ) : nothing ? (
        <Panel padded={false}>
          <EmptyState
            icon="streak"
            title="No streaks yet"
            description="A streak needs at least one day with a listen on it. Import your history, or connect Spotify and let a sync bring today in."
            action={
              <ButtonLink to="/imports" variant="primary">
                Import your history
              </ButtonLink>
            }
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={3}>
            <Stat
              label="Current streak"
              value={current ? formatCount(current.days) : '0'}
              suffix={current && current.days === 1 ? 'day' : 'days'}
              lamp
              loading={streaks.isPending}
              hint={
                streaks.isPending
                  ? 'Counting your days'
                  : current
                    ? `Running since ${dayLabel(current.startDay)}`
                    : 'Nothing played yesterday or today'
              }
            />
            <Stat
              label="Longest ever"
              value={longest ? formatCount(longest.days) : '0'}
              suffix={longest && longest.days === 1 ? 'day' : 'days'}
              loading={streaks.isPending}
              hint={
                streaks.isPending
                  ? 'Reading your whole history'
                  : longest
                    ? `${dayLabel(longest.startDay)} to ${dayLabel(longest.endDay)}`
                    : 'No run recorded yet'
              }
            />
            <Stat
              label="Days in your best runs"
              value={formatCount(top.reduce((sum, streak) => sum + streak.days, 0))}
              loading={streaks.isPending}
              hint={
                streaks.isPending
                  ? 'Adding them up'
                  : `Across the ${formatCount(top.length)} runs below`
              }
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <Panel title="Current streak" description="One cell per day, most recent on the right">
              {streaks.isPending ? (
                <div className="h-16" aria-hidden="true" />
              ) : current ? (
                <StreakBand streak={current} />
              ) : (
                <p className="text-sm text-ink-muted">
                  No streak is running. Play something today and a new one starts at one day.
                </p>
              )}
            </Panel>

            <Panel title="Longest streak" description="One cell per day, most recent on the right">
              {streaks.isPending ? (
                <div className="h-16" aria-hidden="true" />
              ) : longest ? (
                <StreakBand streak={longest} />
              ) : (
                <p className="text-sm text-ink-muted">No run has been recorded yet.</p>
              )}
            </Panel>
          </div>

          <Panel
            title="Longest runs"
            description="Your best streaks, longest first. The one you are on is marked."
            padded={false}
          >
            {streaks.isPending ? (
              <SkeletonLedger rows={5} columns={4} />
            ) : top.length === 0 ? (
              <EmptyState
                icon="streak"
                title="No runs to list"
                description="Streaks appear here as soon as you have two consecutive days with a listen."
              />
            ) : (
              <Ledger caption="Your longest runs of consecutive listening days">
                <LedgerHead>
                  <LedgerRow>
                    <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
                    <LedgerHeaderCell>Run</LedgerHeaderCell>
                    <LedgerHeaderCell numeric>Days</LedgerHeaderCell>
                    <LedgerHeaderCell className="w-1/3">Length</LedgerHeaderCell>
                  </LedgerRow>
                </LedgerHead>
                <LedgerBody>
                  {top.map((streak, index) => (
                    <LedgerRow
                      key={`${streak.startDay}-${streak.endDay}`}
                      current={sameStreak(streak, current)}
                    >
                      <LedgerCell>
                        <LedgerRank rank={index + 1} />
                      </LedgerCell>
                      <LedgerRowHeader className="whitespace-nowrap">
                        {dayLabel(streak.startDay)} to {dayLabel(streak.endDay)}
                        {sameStreak(streak, current) ? (
                          <Chip tone="lamp" className="ml-2">
                            Running
                          </Chip>
                        ) : null}
                      </LedgerRowHeader>
                      <LedgerCell numeric>{formatCount(streak.days)}</LedgerCell>
                      <LedgerCell>
                        {/* The figure beside it is the value; this only shows
                            the runs against each other, so it is decoration. */}
                        <span className="meter block" aria-hidden="true">
                          <span
                            style={{
                              width: `${Math.round((streak.days / Math.max(longest?.days ?? streak.days, 1)) * 100)}%`,
                            }}
                          />
                        </span>
                      </LedgerCell>
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

/**
 * A streak drawn as its days.
 *
 * Every cell is a day that had a listen on it — that is what being inside a
 * streak means — so the band is uniform by definition and carries no value of
 * its own. It is hidden from assistive technology and the sentence under it
 * says the same thing in words.
 */
function StreakBand({ streak }: { streak: Streak }): ReactElement {
  const days = daysOf(streak, BAND_DAYS)
  const hidden = Math.max(streak.days - days.length, 0)

  return (
    <div>
      <div className="flex flex-wrap items-center gap-1" aria-hidden="true">
        {hidden > 0 ? <span className="text-xs text-ink-faint">+{formatCount(hidden)}</span> : null}
        {days.map((day) => (
          <span key={day} className="h-4 w-4 rounded-[2px] bg-lamp" title={dayLabel(day)} />
        ))}
      </div>
      <p className="mt-3 text-sm text-ink-muted">
        <span className="tabular font-semibold text-ink">{formatPlural(streak.days, 'day')}</span>
        {', '}
        {dayLabel(streak.startDay)} to {dayLabel(streak.endDay)}
        {hidden > 0 ? `. The ${formatCount(hidden)} earliest days are not drawn.` : '.'}
      </p>
    </div>
  )
}
