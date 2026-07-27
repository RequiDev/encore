/**
 * Listening sessions: the runs, not the individual plays.
 *
 * A session is a stretch of listening with no silence longer than half an hour
 * in it, which the server derives from the listens themselves. That definition
 * is stated on the page rather than assumed, because "my longest session" is
 * only a meaningful figure if the reader knows what breaks one.
 *
 * Each row expands to its track list. A native `<details>` does that work: it is
 * a focus stop, it opens on Enter and Space, its state is announced, and it
 * needs no JavaScript at all — so a page of fifty of them stays cheap and the
 * browser's own find-in-page can still reach the closed ones.
 */

import type { ReactElement } from 'react'
import { useState } from 'react'

import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import {
  formatClock,
  formatCount,
  formatDateTime,
  formatDuration,
  formatPlural,
  formatTimeOfDay,
} from '../lib/format'
import type { ListeningSession } from '../lib/types'
import {
  Button,
  ButtonLink,
  EmptyState,
  ErrorState,
  Field,
  Icon,
  PageHeader,
  Panel,
  RangeLink,
  RangePicker,
  Select,
  Skeleton,
  Stat,
  StatGrid,
} from '../components/ui'

/** How many sessions to ask for. Ten is the server's own default. */
const LIMITS = [10, 25, 50] as const

export default function Sessions(): ReactElement {
  const { range, label, timeZone, setPreset } = useRange()
  const [limit, setLimit] = useState<number>(10)

  const sessions = useQuery({
    queryKey: qk.listeningSessions(range, limit),
    queryFn: ({ signal }) =>
      api.get<ListeningSession[]>(
        '/stats/sessions',
        { from: range.from, to: range.to, limit },
        signal,
      ),
  })

  const items = sessions.data ?? []
  const longest = items.reduce((best, item) => Math.max(best, item.msPlayed), 0)
  const mostTracks = items.reduce((best, item) => Math.max(best, item.trackCount), 0)
  const total = items.reduce((sum, item) => sum + item.msPlayed, 0)

  const status = sessions.isPending
    ? `Loading your longest sessions for ${label.toLowerCase()}.`
    : sessions.isError
      ? 'Your listening sessions could not be loaded.'
      : items.length === 0
        ? `No listening sessions in ${label.toLowerCase()}.`
        : `${formatPlural(items.length, 'session')} listed, the longest ${formatDuration(longest)}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title="Listening sessions"
        description={`Your longest unbroken runs of listening — ${label.toLowerCase()}.`}
        actions={
          <>
            <Field label="How many sessions to show" labelHidden className="w-32">
              <Select value={limit} onChange={(event) => setLimit(Number(event.target.value))}>
                {LIMITS.map((option) => (
                  <option key={option} value={option}>
                    Top {option}
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

      <StatGrid columns={3}>
        <Stat
          label="Longest session"
          value={formatDuration(longest)}
          lamp
          loading={sessions.isPending}
          hint="Unbroken listening, with no gap over 30 minutes"
        />
        <Stat
          label="Most tracks in one"
          value={formatCount(mostTracks)}
          loading={sessions.isPending}
          hint="Played back to back"
        />
        <Stat
          label="These sessions together"
          value={formatDuration(total)}
          loading={sessions.isPending}
          hint={
            sessions.isPending
              ? 'Adding them up'
              : `Across the ${formatCount(items.length)} listed below`
          }
        />
      </StatGrid>

      <Panel
        title="Longest sessions"
        description="A session ends when you stop for more than 30 minutes. Open one to see what played."
        padded={false}
      >
        {sessions.isPending ? (
          <div className="divide-y divide-seam" aria-busy="true">
            {Array.from({ length: 6 }, (_, i) => (
              <div key={i} className="flex items-center gap-4 px-4 py-3.5">
                <Skeleton className="h-4 flex-1" />
                <Skeleton className="h-4 w-20" />
                <Skeleton className="h-4 w-12" />
              </div>
            ))}
          </div>
        ) : sessions.isError ? (
          <ErrorState
            error={sessions.error}
            title="Your listening sessions could not be loaded"
            onRetry={() => {
              void sessions.refetch()
            }}
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon="session"
            title="No sessions in this range"
            description="A session needs at least one listen. Widen the date range, or import more of your history."
            action={
              <div className="flex flex-wrap items-center justify-center gap-2">
                <Button variant="primary" onClick={() => setPreset('all')}>
                  Show all time
                </Button>
                <ButtonLink to="/imports">Import your history</ButtonLink>
              </div>
            }
          />
        ) : (
          <ol className="divide-y divide-seam">
            {items.map((session, index) => (
              <li key={`${session.startedAt}-${index}`}>
                <SessionRow session={session} rank={index + 1} timeZone={timeZone} />
              </li>
            ))}
          </ol>
        )}
      </Panel>
    </div>
  )
}

/** One session: a summary row that opens onto the tracks it was made of. */
function SessionRow({
  session,
  rank,
  timeZone,
}: {
  session: ListeningSession
  rank: number
  timeZone: string
}): ReactElement {
  const started = formatDateTime(session.startedAt, timeZone)
  const ended = formatTimeOfDay(session.endedAt, timeZone)

  return (
    <details className="group">
      <summary className="flex cursor-pointer list-none flex-wrap items-baseline gap-x-4 gap-y-1 px-4 py-3.5 hover:bg-panel-raised [&::-webkit-details-marker]:hidden">
        <span className="tabular w-6 shrink-0 text-ink-faint">
          {rank.toString().padStart(2, '0')}
        </span>
        <span className="min-w-0 flex-1">
          <time dateTime={session.startedAt} className="tabular text-sm text-ink">
            {started}
          </time>
          <span className="tabular text-sm text-ink-faint"> to {ended}</span>
        </span>
        <span className="tabular text-sm font-semibold text-ink">
          {formatDuration(session.msPlayed)}
        </span>
        <span className="tabular w-24 shrink-0 text-right text-sm text-ink-muted">
          {formatPlural(session.trackCount, 'track')}
        </span>
        <span
          aria-hidden="true"
          className="text-ink-faint transition-transform group-open:rotate-180"
        >
          <Icon name="chevron-down" />
        </span>
      </summary>

      <div className="border-t border-seam bg-chassis px-4 py-3">
        {session.tracks.length === 0 ? (
          <p className="text-sm text-ink-muted">
            The tracks in this session are still being fetched from Spotify.
          </p>
        ) : (
          <ol className="space-y-1.5">
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
        )}
      </div>
    </details>
  )
}
