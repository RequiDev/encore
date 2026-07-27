/**
 * One track.
 *
 * The page answers a single question — how much have I played this? — so the
 * figures come first and the catalogue metadata sits beside them as a legend
 * rather than as the subject. Everything is scoped to the shared URL range, so
 * a link to this page shows the same period to whoever opens it.
 */

import type { ReactElement, ReactNode } from 'react'
import { useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { EMPTY, formatClock, formatPlural } from '../lib/format'
import type { TrackDetail as TrackPayload } from '../lib/types'
import {
  ButtonLink,
  Chip,
  ErrorState,
  PageHeader,
  Panel,
  RangeLink,
  RangePicker,
  Skeleton,
  SkeletonText,
} from '../components/ui'
import { Artwork, EntityFigures, formatRelease } from './top/TopList'

export default function TrackDetail(): ReactElement {
  const { id = '' } = useParams<{ id: string }>()
  const { range, label, timeZone } = useRange()

  const query = useQuery({
    queryKey: qk.track(id, range),
    queryFn: ({ signal }) =>
      api.get<TrackPayload>(
        `/tracks/${encodeURIComponent(id)}`,
        { from: range.from, to: range.to },
        signal,
      ),
    enabled: id !== '',
  })

  const track = query.data?.track
  const stats = query.data?.stats
  const title = track?.name ?? 'Track'
  const notFound = query.error instanceof ApiError && query.error.isNotFound

  const status = query.isPending
    ? 'Loading this track.'
    : query.isError
      ? 'This track could not be loaded.'
      : `${title}: ${formatPlural(stats?.plays ?? 0, 'play')} in ${label.toLowerCase()}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title={title}
        documentTitle={track ? `${track.name} — track` : 'Track'}
        description={`Your plays of this track, ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {query.isPending ? (
        <LoadingBody />
      ) : query.isError || !track || !stats ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title={
              notFound ? 'That track is not in your history' : 'This track could not be loaded'
            }
            onRetry={
              notFound
                ? undefined
                : () => {
                    void query.refetch()
                  }
            }
          >
            {notFound ? (
              <ButtonLink to="/search" variant="primary">
                Search for a track
              </ButtonLink>
            ) : null}
          </ErrorState>
        </Panel>
      ) : (
        <>
          <Panel title="Track">
            <div className="flex flex-wrap items-start gap-4">
              <Artwork src={track.album?.imageUrl ?? ''} kind="album" size={96} />
              <dl className="grid min-w-0 flex-1 gap-3 sm:grid-cols-2">
                <Entry label="Artists">
                  {track.artists.length === 0 ? (
                    <span className="text-ink-muted">{EMPTY}</span>
                  ) : (
                    <ul className="flex flex-wrap gap-x-2 gap-y-1">
                      {track.artists.map((artist) => (
                        <li key={artist.id}>
                          <RangeLink
                            to={`/artists/${artist.id}`}
                            className="text-ink hover:text-lamp"
                          >
                            {artist.name}
                          </RangeLink>
                        </li>
                      ))}
                    </ul>
                  )}
                </Entry>
                <Entry label="Album">
                  {track.album ? (
                    <RangeLink
                      to={`/albums/${track.album.id}`}
                      className="text-ink hover:text-lamp"
                    >
                      {track.album.name}
                    </RangeLink>
                  ) : (
                    // A listen imported from an export can name a track without
                    // ever naming its album; that is a gap in the data, not an error.
                    <span className="text-ink-muted">Not known</span>
                  )}
                </Entry>
                <Entry label="Length">
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="tabular text-ink">{formatClock(track.durationMs)}</span>
                    {track.explicit ? (
                      <Chip tone="warn">
                        <span aria-hidden="true">Explicit</span>
                        <span className="sr-only">Marked explicit on Spotify</span>
                      </Chip>
                    ) : null}
                  </span>
                </Entry>
                <Entry label="Released">
                  <span className="tabular text-ink">
                    {track.album
                      ? formatRelease(track.album.releaseDate, track.album.releasePrecision)
                      : EMPTY}
                  </span>
                </Entry>
              </dl>
            </div>
          </Panel>

          <EntityFigures
            stats={stats}
            timeZone={timeZone}
            subject={track.name}
            busy={query.isFetching}
          />
        </>
      )}
    </div>
  )
}

/** One label-and-value pair in the identity panel. */
function Entry({ label, children }: { label: string; children: ReactNode }): ReactElement {
  return (
    <div className="min-w-0">
      <dt className="eyebrow">{label}</dt>
      <dd className="mt-1 min-w-0 text-sm">{children}</dd>
    </div>
  )
}

/** The page's shape while the one request is in flight, so nothing jumps. */
function LoadingBody(): ReactElement {
  return (
    <div className="space-y-4" role="status" aria-live="polite" aria-busy="true">
      <span className="sr-only">Loading this track</span>
      <Panel title="Track">
        <div className="flex items-start gap-4">
          <Skeleton className="h-24 w-24" />
          <SkeletonText lines={4} className="max-w-md flex-1" />
        </div>
      </Panel>
      <div className="panel h-28" />
      <div className="panel h-72" />
    </div>
  )
}
