/**
 * A shared page: somebody's listening, shown to whoever holds the link.
 *
 * It is deliberately not the dashboard with the navigation hidden. A visitor is
 * not a user of this instance — they cannot change the range, follow a link into
 * an artist page, or discover that anything else exists here — so the page is
 * flat, self-explanatory and finite. It says whose it is, what period it covers,
 * and then shows the figures.
 *
 * What it cannot show is the point: there is no listening history and no way to
 * ask for one. The server composes a fixed payload for exactly this reason.
 */

import type { ReactElement, ReactNode } from 'react'
import { useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import type { SharedStats } from '../lib/types'
import { formatCount, formatDate, formatDurationShort, formatPlural, formatRatio } from '../lib/format'
import { useDocumentTitle } from '../lib/hooks'
import { ChartCard, HourChart, TimelineChart, WeekdayChart } from '../components/charts'
import {
  EmptyState,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerRank,
  LedgerRow,
  LedgerRowHeader,
  Panel,
  Skeleton,
  Stat,
  StatGrid,
} from '../components/ui'

export default function Share(): ReactElement {
  const { token = '' } = useParams()

  const shared = useQuery({
    queryKey: qk.sharedStats(token),
    queryFn: ({ signal }) =>
      api.get<SharedStats>(`/share/${encodeURIComponent(token)}`, undefined, signal),
    // A dead link is dead. Retrying a 404 only delays telling the visitor.
    retry: false,
    enabled: token !== '',
  })

  const title = shared.data
    ? `${shared.data.label || 'Listening'} · ${shared.data.displayName}`
    : 'Shared listening'
  useDocumentTitle(title)

  if (shared.isPending) {
    return (
      <Frame>
        <div className="space-y-5" role="status" aria-busy="true" aria-live="polite">
          <span className="sr-only">Loading</span>
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      </Frame>
    )
  }

  if (shared.isError) {
    return (
      <Frame>
        <Panel padded={false}>
          <EmptyState
            icon="settings"
            title="This link does not work"
            description="It may have been revoked by whoever created it, or it may have expired. Ask them for a new one — there is nothing to fix at your end."
          />
        </Panel>
      </Frame>
    )
  }

  const data = shared.data
  const { summary } = data

  return (
    <Frame>
      <header className="border-b border-seam pb-5">
        <p className="eyebrow">Shared listening</p>
        <h1 className="mt-1 text-2xl font-semibold text-ink">
          {data.label || `${data.displayName}’s listening`}
        </h1>
        <p className="mt-2 text-sm text-ink-muted">
          {data.displayName} ·{' '}
          <span className="tabular">
            {data.rolling
              ? `the last ${formatPlural(data.rangeDays, 'day')}`
              : `${formatDate(data.from, data.timezone)} – ${formatDate(data.to, data.timezone)}`}
          </span>
        </p>
      </header>

      {summary.listens === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon="dashboard"
            title="Nothing in this period"
            description="The link works — there is simply no listening recorded for the dates it covers."
          />
        </Panel>
      ) : (
        <>
          <StatGrid columns={4}>
            <Stat label="Listens" value={formatCount(summary.listens)} />
            <Stat label="Listening time" value={formatDurationShort(summary.msPlayed)} />
            <Stat label="Different tracks" value={formatCount(summary.distinctTracks)} />
            <Stat label="Different artists" value={formatCount(summary.distinctArtists)} />
          </StatGrid>

          <ChartCard title="Over time" description="Plays per bucket across the shared period.">
            <TimelineChart
              buckets={data.timeline}
              interval={data.interval}
              timeZone={data.timezone}
              metric="plays"
            />
          </ChartCard>

          <div className="grid gap-5 lg:grid-cols-2">
            <ChartCard title="By hour" description="When these plays happened, locally.">
              <HourChart buckets={data.hours} />
            </ChartCard>
            <ChartCard title="By weekday" description="Monday first.">
              <WeekdayChart buckets={data.weekdays} />
            </ChartCard>
          </div>

          <div className="grid gap-5 lg:grid-cols-2">
            <TopList
              title="Top artists"
              rows={data.artists.items.map((e) => ({
                key: e.entity.id,
                rank: e.rank,
                name: e.entity.name,
                plays: e.plays,
              }))}
            />
            <TopList
              title="Top tracks"
              rows={data.tracks.items.map((e) => ({
                key: e.entity.id,
                rank: e.rank,
                name: e.entity.name,
                detail: e.entity.artists.map((a) => a.name).join(', '),
                plays: e.plays,
              }))}
            />
          </div>

          <TopList
            title="Top albums"
            rows={data.albums.items.map((e) => ({
              key: e.entity.id,
              rank: e.rank,
              name: e.entity.name,
              plays: e.plays,
            }))}
          />

          {/*
           * Genres and taste: the same data class as the top lists above —
           * what somebody listens to, not how or where — so they are on a
           * share too, each with its own coverage sentence like every other
           * partial statistic in Encore. Playback context (skip, shuffle,
           * platform, country, offline, incognito) stays off every share;
           * an end-to-end test asserts its field names never appear here.
           */}
          {data.genres ? (
            <>
              <TopList
                title="Top genres"
                rows={data.genres.genres.map((g, i) => ({
                  key: g.genre,
                  rank: i + 1,
                  name: g.genre,
                  plays: g.plays,
                }))}
              />
              {data.genres.genres.length > 0 ? (
                <p className="text-xs text-ink-faint">
                  {data.genres.coverage.covered === data.genres.coverage.total
                    ? 'Genres are known for all of this listening.'
                    : `Genres are known for ${formatRatio(data.genres.coverage.covered, data.genres.coverage.total)} of this listening — a track counts toward each of its genres, so these add up to more than the total plays above.`}
                </p>
              ) : null}
            </>
          ) : null}

          {data.taste ? (
            <StatGrid columns={2}>
              <Stat
                label="Obscurity"
                value={formatCount(data.taste.obscurity.value)}
                suffix="of 100"
                hint={tasteHint(data.taste.obscurity.covered, data.taste.obscurity.total)}
              />
              <Stat
                label="Release lag"
                value={formatYears(data.taste.releaseLag.value)}
                suffix="years old"
                hint={tasteHint(data.taste.releaseLag.covered, data.taste.releaseLag.total)}
              />
            </StatGrid>
          ) : null}
        </>
      )}

      <footer className="border-t border-seam pt-5 text-xs text-ink-faint">
        Shared from a private Encore instance. This page shows totals and rankings only — never
        individual plays or when they happened.
      </footer>
    </Frame>
  )
}

/**
 * The page chrome. A share is standalone: no navigation, no search, nothing that
 * would imply the visitor has an account here.
 */
function Frame({ children }: { children: ReactNode }): ReactElement {
  return (
    <div className="min-h-screen bg-page">
      <main id="main" className="mx-auto max-w-5xl space-y-5 px-4 py-8 sm:px-6">
        {children}
      </main>
    </div>
  )
}

interface TopRow {
  key: string
  rank: number
  name: string
  detail?: string
  plays: number
}

/** A ranked list, read-only: nothing here links anywhere. */
function TopList({ title, rows }: { title: string; rows: TopRow[] }): ReactElement {
  return (
    <Panel title={title} padded={false}>
      {rows.length === 0 ? (
        <EmptyState icon="track" title="Nothing to rank" description="No plays in this period." />
      ) : (
        <Ledger caption={title}>
          <LedgerBody>
            {rows.map((row) => (
              <LedgerRow key={row.key}>
                <LedgerCell>
                  <LedgerRank rank={row.rank} />
                </LedgerCell>
                <LedgerRowHeader>
                  <span className="text-ink">{row.name || 'Not yet named'}</span>
                  {row.detail ? (
                    <span className="block text-xs text-ink-muted">{row.detail}</span>
                  ) : null}
                </LedgerRowHeader>
                <LedgerCell numeric>
                  <span className="tabular">{formatCount(row.plays)}</span>
                </LedgerCell>
              </LedgerRow>
            ))}
          </LedgerBody>
        </Ledger>
      )}
    </Panel>
  )
}

/** What a taste score's coverage reads, over the share's fixed period. */
function tasteHint(covered: number, total: number): string {
  if (total <= 0) return 'Not known for this period'
  return `Known for ${formatRatio(covered, total)} of this listening`
}

/**
 * A release-lag figure is a count of years, not a ratio — there is no
 * percentage formatter that would be right here.
 */
function formatYears(value: number): string {
  if (!Number.isFinite(value)) return '0.0'
  return value.toFixed(1)
}
