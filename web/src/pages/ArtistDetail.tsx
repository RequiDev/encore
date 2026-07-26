/**
 * One artist.
 *
 * Everything the track page has, plus the three things only an artist has: what
 * share of the range they account for, what you actually play by them, and the
 * switch that takes them out of your statistics.
 *
 * The blacklist toggle is a mutation with a written explanation beside it rather
 * than an icon with a tooltip. It changes every number on every other page, and
 * a control with that reach has to say what it does before it is pressed.
 */

import type { ReactElement, ReactNode } from 'react'
import { useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { formatCount, formatDuration, formatPlural } from '../lib/format'
import type { ArtistDetail as ArtistPayload } from '../lib/types'
import {
  Button,
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  PageHeader,
  Panel,
  RangePicker,
  Skeleton,
  SkeletonLedger,
  SkeletonText,
  errorMessage,
  useToast,
} from '../components/ui'
import { ChartCard, HourChart, ShareBar } from '../components/charts'
import { Artwork, EntityFigures, EntityLedger, formatRelease } from './top/TopList'

export default function ArtistDetail(): ReactElement {
  const { id = '' } = useParams<{ id: string }>()
  const { range, label, timeZone } = useRange()
  const queryClient = useQueryClient()
  const toast = useToast()

  const query = useQuery({
    queryKey: qk.artist(id, range),
    queryFn: ({ signal }) =>
      api.get<ArtistPayload>(
        `/artists/${encodeURIComponent(id)}`,
        { from: range.from, to: range.to },
        signal,
      ),
    enabled: id !== '',
  })

  const artist = query.data?.artist
  const stats = query.data?.stats
  const blacklisted = query.data?.blacklisted ?? false
  const title = artist?.name ?? 'Artist'
  const notFound = query.error instanceof ApiError && query.error.isNotFound

  const toggle = useMutation({
    mutationFn: async (exclude: boolean): Promise<boolean> => {
      if (exclude) await api.post<void>('/blacklist', { artistId: id })
      else await api.del<void>(`/blacklist/${encodeURIComponent(id)}`)
      return exclude
    },
    onSuccess: (exclude) => {
      toast.notify({
        tone: 'success',
        title: exclude ? `${title} no longer counts` : `${title} counts again`,
        description: exclude
          ? 'Their plays are excluded from your statistics. Nothing was deleted.'
          : 'Their plays are back in your totals, top lists and charts.',
      })
      // Every statistic in the cache is now wrong, and so is every entity page:
      // one blacklisted artist changes totals, ranks and shares everywhere.
      void queryClient.invalidateQueries({
        predicate: (cached) => cached.queryKey[0] === 'stats' || cached.queryKey[0] === 'entity',
      })
      void queryClient.invalidateQueries({ queryKey: qk.blacklist() })
    },
    onError: (error) => {
      toast.notify({
        tone: 'error',
        title: 'That change did not save',
        description: errorMessage(error),
      })
    },
  })

  const status = query.isPending
    ? 'Loading this artist.'
    : query.isError
      ? 'This artist could not be loaded.'
      : `${title}: ${formatPlural(stats?.plays ?? 0, 'play')} in ${label.toLowerCase()}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title={title}
        documentTitle={artist ? `${artist.name} — artist` : 'Artist'}
        description={`Your listening to this artist, ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {query.isPending ? (
        <LoadingBody />
      ) : query.isError || !artist || !stats || !query.data ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title={
              notFound ? 'That artist is not in your history' : 'This artist could not be loaded'
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
                Search for an artist
              </ButtonLink>
            ) : null}
          </ErrorState>
        </Panel>
      ) : (
        <>
          <div className="grid gap-4 lg:grid-cols-3">
            <Panel title="Artist" className="lg:col-span-2">
              <div className="flex flex-wrap items-start gap-4">
                <Artwork src={artist.imageUrl} kind="artist" size={96} />
                <dl className="grid min-w-0 flex-1 gap-3 sm:grid-cols-2">
                  <Entry label="Genres">
                    {artist.genres.length === 0 ? (
                      <span className="text-ink-muted">None on record</span>
                    ) : (
                      <ul className="flex flex-wrap gap-1.5">
                        {artist.genres.map((genre) => (
                          <li key={genre}>
                            <Chip>{genre}</Chip>
                          </li>
                        ))}
                      </ul>
                    )}
                  </Entry>
                  <Entry label="Followers on Spotify">
                    <span className="tabular text-ink">{formatCount(artist.followers)}</span>
                  </Entry>
                  <Entry label="Popularity">
                    <span className="tabular text-ink">{formatCount(artist.popularity)}</span>
                    <span className="text-ink-muted"> / 100</span>
                    <span className="mt-1 block text-xs text-ink-faint">
                      Spotify&rsquo;s own measure of how much everyone plays them, not you.
                    </span>
                  </Entry>
                  <Entry label="In your statistics">
                    {blacklisted ? (
                      <Chip tone="warn">Excluded</Chip>
                    ) : (
                      <span className="text-ink">Counted</span>
                    )}
                  </Entry>
                </dl>
              </div>
            </Panel>

            <Panel
              title="Share of your listening"
              description="Of your total listening time in this range"
            >
              <ShareBar
                value={query.data.share}
                total={1}
                label={`${artist.name}, share of your listening time`}
                detail={`${formatDuration(stats.msPlayed)} of your listening in ${label.toLowerCase()}`}
              />
            </Panel>
          </div>

          <EntityFigures
            stats={stats}
            timeZone={timeZone}
            subject={artist.name}
            busy={query.isFetching}
          />

          <div className="grid gap-4 lg:grid-cols-2">
            <Panel
              title="Top tracks"
              description={`By ${artist.name}, ranked by plays`}
              padded={false}
            >
              {query.data.topTracks.length === 0 ? (
                <EmptyState
                  icon="track"
                  title="No tracks in this range"
                  description="You did not play anything by this artist between these dates. Widen the range above."
                />
              ) : (
                <EntityLedger
                  caption={`Tracks by ${artist.name} you played in this range, ranked by plays`}
                  column="Track"
                  kind="track"
                  rows={query.data.topTracks.map((entry) => ({
                    key: entry.entity.id,
                    to: `/tracks/${entry.entity.id}`,
                    name: entry.entity.name,
                    imageUrl: entry.entity.album?.imageUrl ?? '',
                    meta: entry.entity.album?.name ?? '',
                    plays: entry.plays,
                    msPlayed: entry.msPlayed,
                    rank: entry.rank,
                  }))}
                />
              )}
            </Panel>

            <Panel
              title="Top albums"
              description={`By ${artist.name}, ranked by plays`}
              padded={false}
            >
              {query.data.topAlbums.length === 0 ? (
                <EmptyState
                  icon="album"
                  title="No albums in this range"
                  description="Nothing by this artist has an album on record between these dates."
                />
              ) : (
                <EntityLedger
                  caption={`Albums by ${artist.name} you played in this range, ranked by plays`}
                  column="Album"
                  kind="album"
                  rows={query.data.topAlbums.map((entry) => ({
                    key: entry.entity.id,
                    to: `/albums/${entry.entity.id}`,
                    name: entry.entity.name,
                    imageUrl: entry.entity.imageUrl,
                    meta: formatRelease(entry.entity.releaseDate, entry.entity.releasePrecision),
                    plays: entry.plays,
                    msPlayed: entry.msPlayed,
                    rank: entry.rank,
                  }))}
                />
              )}
            </Panel>
          </div>

          <ChartCard
            title="Hour of day"
            description={`When you play ${artist.name}, in your timezone`}
          >
            <HourChart buckets={query.data.hourRepartition} busy={query.isFetching} />
          </ChartCard>

          <Panel title="Blacklist" description="Leave an artist out of every statistic">
            <div className="flex flex-col gap-4 md:flex-row md:items-start md:justify-between">
              <div className="max-w-prose space-y-2 text-sm text-ink-muted">
                <p>
                  Blacklisting an artist stops them counting towards your statistics: totals, top
                  lists, charts and the dashboard all ignore their plays.
                </p>
                <p>
                  Nothing is deleted. Every listen stays in your history, and putting the artist
                  back restores the figures exactly as they were.
                </p>
                <p role="status" aria-live="polite" className="text-ink">
                  {blacklisted
                    ? `${artist.name} is excluded from your statistics.`
                    : `${artist.name} counts towards your statistics.`}
                </p>
              </div>
              <Button
                variant={blacklisted ? 'primary' : 'default'}
                busy={toggle.isPending}
                onClick={() => toggle.mutate(!blacklisted)}
                className="shrink-0 self-start"
              >
                {blacklisted ? 'Count this artist again' : 'Exclude from statistics'}
              </Button>
            </div>
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
      <span className="sr-only">Loading this artist</span>
      <div className="grid gap-4 lg:grid-cols-3">
        <Panel title="Artist" className="lg:col-span-2">
          <div className="flex items-start gap-4">
            <Skeleton className="h-24 w-24 rounded-full" />
            <SkeletonText lines={4} className="max-w-md flex-1" />
          </div>
        </Panel>
        <Panel title="Share of your listening">
          <SkeletonText lines={2} />
        </Panel>
      </div>
      <div className="panel h-28" />
      <div className="panel h-72" />
      <div className="grid gap-4 lg:grid-cols-2">
        <Panel padded={false}>
          <SkeletonLedger rows={5} columns={4} />
        </Panel>
        <Panel padded={false}>
          <SkeletonLedger rows={5} columns={4} />
        </Panel>
      </div>
    </div>
  )
}
