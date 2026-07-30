/**
 * One album.
 *
 * The track list is the point of this page: which songs off a record you
 * actually play, and how the plays are spread across it. Only tracks with plays
 * in the range come back from the API, so the panel says how many of the
 * album's tracks that is rather than implying the rest do not exist.
 */

import type { ReactElement, ReactNode } from 'react'
import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { EMPTY, formatCount, formatDate, formatPlural } from '../lib/format'
import type {
  AlbumCompletion,
  AlbumDetail as AlbumPayload,
  AlbumTrackList,
  AlbumTrackListState,
} from '../lib/types'
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
  Stat,
} from '../components/ui'
import { Artwork, EntityFigures, EntityLedger, formatRelease } from './top/TopList'

/**
 * How often the page asks again while Spotify is being read.
 *
 * Two seconds is short enough that the list appears while somebody is still
 * looking at the page and long enough not to be a load generator.
 */
const TRACKLIST_POLL_MS = 2000

/**
 * How long this page keeps asking before it stops.
 *
 * The cap is not an optimisation, it is the only thing that ends the poll.
 * "pending" is unbounded server-side: a claim against `album_track_fetches`
 * that errors records nothing, so the very next request re-enters the same
 * branch, and every "pending" response for one album is byte-identical however
 * long the state has held — second two and hour two are indistinguishable from
 * here. Without this, a read-only replica or a full tablespace turns every open
 * album tab into a request every two seconds, for ever.
 *
 * Two minutes is chosen against the server's own numbers rather than picked:
 * one album's whole walk is bounded at ninety seconds and a lease stranded by a
 * killed process expires after two, so any fetch that is genuinely going to
 * resolve has resolved by here. Sixty requests is the worst this page can cost
 * one album.
 */
const TRACKLIST_POLL_CAP_MS = 120_000

/**
 * Key prefix for when this tab first saw `pending` for an album.
 *
 * Exported for the test that reloads a page near the cap.
 */
export const TRACKLIST_POLL_START_KEY = 'encore.tracklist-poll-start.'

/**
 * The next poll delay, or `false` to stop.
 *
 * Exported so it can be tested without driving a real timer through TanStack
 * Query. `gaveUp` is the cap above having passed; everything other than a
 * running fetch stops immediately, and "disabled" never polls at all because
 * there is no fetch to wait for.
 */
export function tracklistPollInterval(
  state: AlbumTrackListState | undefined,
  gaveUp = false,
): number | false {
  return state === 'pending' && !gaveUp ? TRACKLIST_POLL_MS : false
}

/**
 * When this tab first saw `pending` for this album, in epoch milliseconds,
 * recording `now` the first time it is asked.
 *
 * In `sessionStorage` rather than component state on purpose: an in-memory
 * clock restarts with the component, so somebody who reloads a stuck page a few
 * seconds before the cap gets a fresh two minutes every time and the cap never
 * arrives. Anything unreadable — private browsing, storage disabled — falls
 * back to now, which still caps the poll, just per page load.
 */
function pollStartedAt(albumId: string, now: number): number {
  const key = TRACKLIST_POLL_START_KEY + albumId
  try {
    const stored = Number(window.sessionStorage.getItem(key))
    // A start in the future is a clock that moved under us; treat it as now
    // rather than granting an unbounded window.
    if (Number.isFinite(stored) && stored > 0 && stored <= now) return stored
    window.sessionStorage.setItem(key, String(now))
  } catch {
    // See above: a per-load cap is a much smaller problem than no cap.
  }
  return now
}

function clearPollStart(albumId: string): void {
  try {
    window.sessionStorage.removeItem(TRACKLIST_POLL_START_KEY + albumId)
  } catch {
    // Nothing was stored, so there is nothing to clear.
  }
}

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
  const completion = query.data?.completion
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
                  {album.totalTracks > 0 ? (
                    <span className="tabular text-ink">{formatCount(album.totalTracks)}</span>
                  ) : (
                    <span className="text-ink-muted">Not known</span>
                  )}
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

          <Panel title="Heard" description="How much of this album you have heard." padded={false}>
            <CompletionFigure completion={completion} />
          </Panel>

          {/*
            Keyed by album so that walking from one record to another cannot
            carry the previous one's poll — its own request, its own cap.
          */}
          <NeverPlayedPanel
            key={album.id}
            albumId={album.id}
            totalTracks={album.totalTracks}
            timeZone={timeZone}
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

/**
 * How much of the album has been heard, ever.
 *
 * Every other figure on this page is scoped to the range picker; this one
 * deliberately is not, so it carries "all time" the same way `EntityFigures`
 * carries it for first and last listen above — a hint under the figure,
 * never a second, differently-worded way of saying the same thing.
 *
 * `known` is false until the album's track count has been enriched, which is
 * true of nearly every album on a freshly imported instance. That state
 * cannot compute a ratio at all, so it says so instead of rendering one — a
 * fabricated "0 of 0" or a bare "0%" would read as "you have heard nothing
 * from this record," which is not what an unresolved track count means.
 */
function CompletionFigure({
  completion,
}: {
  completion: AlbumCompletion | undefined
}): ReactElement {
  if (!completion || !completion.known) {
    return (
      <EmptyState
        title="Track count not known yet"
        description={
          <>
            Encore learns track counts from Spotify while it fills in your catalogue; check{' '}
            <Link to="/settings" className="text-lamp hover:underline">
              Settings
            </Link>{' '}
            for progress.
          </>
        }
      />
    )
  }

  // Worth calling out on its own: "12 of 12" buries the one state that is
  // actually interesting to notice.
  const complete = completion.heard >= completion.total

  return (
    <Stat
      label="Tracks heard"
      value={
        complete
          ? 'Every track'
          : `${formatCount(completion.heard)} of ${formatCount(completion.total)}`
      }
      suffix={complete ? undefined : 'tracks'}
      hint="all time"
    />
  )
}

/**
 * The never-played panel, with the request and the poll it needs.
 *
 * Separate from the page's own query for two reasons. Asking for the listing is
 * the only thing that starts a fetch from Spotify, so there is exactly one
 * place that can start one; and this is the only request on the page that
 * repeats, which must not re-run the album's whole statistics on every tick.
 *
 * The cap lives here rather than on the page because it belongs to one album.
 * The page mounts this keyed by album id, so nothing about one record's stuck
 * fetch can follow a reader to the next.
 */
function NeverPlayedPanel({
  albumId,
  totalTracks,
  timeZone,
}: {
  albumId: string
  totalTracks: number
  timeZone: string
}): ReactElement {
  const [gaveUp, setGaveUp] = useState(false)
  const tracklist = useQuery({
    queryKey: qk.albumTracklist(albumId),
    queryFn: ({ signal }) =>
      api.get<AlbumTrackList>(
        `/albums/${encodeURIComponent(albumId)}/tracklist`,
        undefined,
        signal,
      ),
    enabled: albumId !== '',
    refetchInterval: (query) => tracklistPollInterval(query.state.data?.state, gaveUp),
  })
  const list = tracklist.data
  const state = list?.state

  // Stopping the requests is only half of it: the panel has to say it has
  // stopped, and once the poll ends no further response is coming to re-render
  // it. Hence a timer for the moment the cap passes, sized from the persisted
  // start so a reload resumes the same window rather than opening a new one.
  useEffect(() => {
    if (state !== 'pending') {
      // A settled answer closes the window, so an album that returns to
      // "pending" much later — a failure left alone long enough for the server
      // to try again — is given its own full two minutes.
      if (state) clearPollStart(albumId)
      return
    }
    const remaining = pollStartedAt(albumId, Date.now()) + TRACKLIST_POLL_CAP_MS - Date.now()
    const timer = window.setTimeout(() => setGaveUp(true), Math.max(remaining, 0))
    return () => {
      window.clearTimeout(timer)
    }
  }, [albumId, state])

  return (
    <Panel
      title="Tracks you have never played"
      description={
        // The count is stated only when there is one. With nothing missing it
        // becomes "0 of the 12 tracks ... have no plays", which is the same
        // fact the body states below it, phrased as a double negative and said
        // twice.
        list?.state === 'ready' && list.missing.length > 0
          ? `${formatCount(list.missing.length)} of the ${formatPlural(
              list.coverage.total,
              'track',
            )} Spotify lists for this album have no plays in your history, all time.`
          : // Deliberately free of any promise. This same line sits above
            // "pending", "unavailable" and "disabled", and only one of the
            // three is going to resolve into a list.
            "From Spotify's own list of what is on this album, read once and kept."
      }
      padded={false}
    >
      <MissingTracks
        list={list}
        failed={tracklist.isError}
        gaveUp={gaveUp}
        totalTracks={totalTracks}
        timeZone={timeZone}
      />
    </Panel>
  )
}

/**
 * Which tracks on this record have never been played.
 *
 * Everything else on this page is computed from listening Encore already holds.
 * This is not: it needs Spotify's own list of what is on the album, which Encore
 * reads the first time somebody opens this page and then keeps. So an empty list
 * here means one of four different things, and saying which is the whole job:
 *
 *   pending     — Encore has not been told what is on the album yet
 *   unavailable — Encore asked and could not find out
 *   disabled    — this instance does not ask, because its operator said not to
 *   ready       — Encore knows, and you have played all of it
 *
 * `disabled` and `unavailable` are kept apart deliberately: one is somebody
 * here choosing not to ask and the other is Spotify not answering, and telling
 * a person the second when the first is true blames a third party for a local
 * decision. A listing cached before the switch was turned off still arrives as
 * `ready` — turning off fetching does not hide what is already on disk — and
 * the date it was read is rendered on every `ready` state, which is what keeps
 * a list that will never refresh from reading as though it were current.
 *
 * Its denominator is the list Spotify returned, never `album.totalTracks`.
 * Those are two different readings from two different endpoints and they are
 * allowed to disagree; when they do, this says both rather than quietly picking
 * one. It never feeds them back the other way: the completion figure above
 * belongs to enrichment, and an album with no track count stays without one.
 */
function MissingTracks({
  list,
  failed,
  gaveUp,
  totalTracks,
  timeZone,
}: {
  list: AlbumTrackList | undefined
  failed: boolean
  gaveUp: boolean
  totalTracks: number
  timeZone: string
}): ReactElement {
  // The state is checked before the list is, deliberately. Branching on
  // `missing.length` first would render "you have played every track" for an
  // album Encore has not even asked about yet.
  if (list?.state === 'disabled') {
    return (
      <EmptyState
        title="Album track lists are turned off"
        description="This instance does not ask Spotify what is on an album, so Encore cannot say which tracks you have never played. Everything else on this page comes from your own listening and is unaffected. An administrator can turn this on with ENCORE_ALBUM_TRACKS_ENABLED."
      />
    )
  }
  // A poll that hit its cap lands here rather than in the branch below. After
  // two minutes of an answer that carries no clock, "Encore could not get the
  // list" is what a person can act on, and the retry it promises is real: the
  // next view of this album starts a fresh attempt server-side.
  if (failed || list?.state === 'unavailable' || (gaveUp && list?.state === 'pending')) {
    return (
      <EmptyState
        title="This album's track list could not be read"
        description="Encore could not get the list of what is on this album from Spotify, so it cannot say which tracks you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later."
      />
    )
  }
  if (!list || list.state === 'pending') {
    return (
      <EmptyState
        title="Asking Spotify for this album's track list"
        description="Encore reads it once and keeps it, so this happens only the first time somebody opens this album. The list appears here on its own."
      />
    )
  }

  const listed = list.coverage.total
  // Two separate reads of the same album, and the identity panel above is
  // already showing the other one. Silence here would be two different totals
  // on one screen with nothing to tell a reader which to believe — including
  // the very common case of a fresh instance, where the album record has no
  // count at all and this panel would otherwise be the only confident number
  // on a page that has just said "Not known" twice.
  const reconciliation =
    listed === 0 || listed === totalTracks
      ? null
      : `Spotify lists ${formatPlural(listed, 'track')} for this album; the album record ${
          totalTracks > 0 ? `says ${formatCount(totalTracks)}` : 'has no track count yet'
        }. This panel follows the list.`
  // Rendered on every `ready` state, not only when something is missing. On an
  // instance with fetching turned off this list will never change again, and a
  // list with no date on it reads as though it were current.
  const readOn = list.fetchedAt
    ? `Track list read from Spotify on ${formatDate(list.fetchedAt, timeZone)}.`
    : null

  if (list.missing.length === 0) {
    return (
      <div className="px-4 py-3">
        {/*
          The reconciliation replaces the count rather than following it: it
          already names the listing's total, and "All 14 tracks Spotify lists
          for it. Spotify lists 14 tracks for this album; ..." says the same
          number twice in two sentences.
        */}
        <EmptyState
          icon="track"
          title="You have played every track on this album"
          description={reconciliation ?? `All ${formatPlural(listed, 'track')} Spotify lists for it.`}
        />
        {readOn ? <p className="mt-2 text-center text-xs text-ink-faint">{readOn}</p> : null}
      </div>
    )
  }

  return (
    <div className="px-4 py-3">
      <ul className="divide-y divide-seam">
        {list.missing.map((track) => (
          <li key={track.id} className="flex items-baseline gap-3 py-2 text-sm">
            <span className="tabular w-10 shrink-0 text-right text-ink-faint">
              {track.discNumber > 1 ? `${track.discNumber}.` : ''}
              {track.trackNumber}
            </span>
            <span className="min-w-0 flex-1 truncate text-ink">{track.name}</span>
          </li>
        ))}
      </ul>
      {reconciliation ? <p className="mt-3 text-sm text-ink-muted">{reconciliation}</p> : null}
      {readOn ? <p className="mt-2 text-xs text-ink-faint">{readOn}</p> : null}
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
      <div className="panel h-28" />
      <div className="panel h-40" />
      <Panel padded={false}>
        <SkeletonLedger rows={8} columns={4} />
      </Panel>
    </div>
  )
}
