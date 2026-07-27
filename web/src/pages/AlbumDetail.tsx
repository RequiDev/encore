/**
 * One album.
 *
 * The track list is the point of this page: which songs off a record you
 * actually play, and how the plays are spread across it. Only tracks with plays
 * in the range come back from the API, so the panel says how many of the
 * album's tracks that is rather than implying the rest do not exist.
 */

import type { ReactElement, ReactNode } from 'react'
import { useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { EMPTY, formatCount, formatPlural } from '../lib/format'
import type { AlbumDetail as AlbumPayload } from '../lib/types'
import {
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  PageHeader,
  Panel,
  RangeLink,
  RangePicker,
  Skeleton,
  SkeletonLedger,
  SkeletonText,
} from '../components/ui'
import { Artwork, EntityFigures, EntityLedger, formatRelease } from './top/TopList'

export default function AlbumDetail(): ReactElement {
  const { id = '' } = useParams<{ id: string }>()
  const { range, label, timeZone } = useRange()

  const query = useQuery({
    queryKey: qk.album(id, range),
    queryFn: ({ signal }) =>
      api.get<AlbumPayload>(
        `/albums/${encodeURIComponent(id)}`,
        { from: range.from, to: range.to },
        signal,
      ),
    enabled: id !== '',
  })

  const album = query.data?.album
  const stats = query.data?.stats
  const tracks = query.data?.topTracks ?? []
  const title = album?.name ?? 'Album'
  const notFound = query.error instanceof ApiError && query.error.isNotFound

  const status = query.isPending
    ? 'Loading this album.'
    : query.isError
      ? 'This album could not be loaded.'
      : `${title}: ${formatPlural(stats?.plays ?? 0, 'play')} in ${label.toLowerCase()}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title={title}
        documentTitle={album ? `${album.name} — album` : 'Album'}
        description={`Your plays from this album, ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {query.isPending ? (
        <LoadingBody />
      ) : query.isError || !album || !stats ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title={
              notFound ? 'That album is not in your history' : 'This album could not be loaded'
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
                Search for an album
              </ButtonLink>
            ) : null}
          </ErrorState>
        </Panel>
      ) : (
        <>
          <Panel title="Album">
            <div className="flex flex-wrap items-start gap-4">
              <Artwork src={album.imageUrl} kind="album" size={96} />
              <dl className="grid min-w-0 flex-1 gap-3 sm:grid-cols-2">
                <Entry label="Artists">
                  {album.artists.length === 0 ? (
                    <span className="text-ink-muted">{EMPTY}</span>
                  ) : (
                    <ul className="flex flex-wrap gap-x-2 gap-y-1">
                      {album.artists.map((artist) => (
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
                <Entry label="Released">
                  <span className="tabular text-ink">
                    {formatRelease(album.releaseDate, album.releasePrecision)}
                  </span>
                </Entry>
                <Entry label="Tracks">
                  <span className="tabular text-ink">{formatCount(album.totalTracks)}</span>
                </Entry>
                <Entry label="Kind">
                  {album.albumType ? (
                    <Chip>{album.albumType}</Chip>
                  ) : (
                    <span className="text-ink-muted">Not known</span>
                  )}
                </Entry>
              </dl>
            </div>
          </Panel>

          <EntityFigures
            stats={stats}
            timeZone={timeZone}
            subject={album.name}
            busy={query.isFetching}
          />

          <Panel
            title="Tracks you played"
            description={
              album.totalTracks > 0
                ? `${formatCount(tracks.length)} of ${formatPlural(album.totalTracks, 'track')} on this album, ranked by plays`
                : 'Ranked by plays, highest first'
            }
            padded={false}
          >
            {tracks.length === 0 ? (
              <EmptyState
                icon="track"
                title="No tracks in this range"
                description="You did not play anything from this album between these dates. Widen the range above."
              />
            ) : (
              <EntityLedger
                caption={`Tracks from ${album.name} you played in this range, ranked by plays`}
                column="Track"
                kind="track"
                // Every row would carry the same cover, which says nothing.
                artwork={false}
                rows={tracks.map((entry) => ({
                  key: entry.entity.id,
                  to: `/tracks/${entry.entity.id}`,
                  name: entry.entity.name,
                  imageUrl: entry.entity.album?.imageUrl ?? '',
                  meta: entry.entity.artists.map((artist) => artist.name).join(', '),
                  plays: entry.plays,
                  msPlayed: entry.msPlayed,
                  rank: entry.rank,
                }))}
              />
            )}
          </Panel>
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
      <span className="sr-only">Loading this album</span>
      <Panel title="Album">
        <div className="flex items-start gap-4">
          <Skeleton className="h-24 w-24" />
          <SkeletonText lines={4} className="max-w-md flex-1" />
        </div>
      </Panel>
      <div className="panel h-28" />
      <div className="panel h-72" />
      <Panel padded={false}>
        <SkeletonLedger rows={8} columns={4} />
      </Panel>
    </div>
  )
}
