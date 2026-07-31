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
import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { clearPollStart, lazyPollInterval, pollStartKey, pollStartedAt } from '../lib/fetchpoll'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { formatCount, formatDate, formatDuration, formatPlural } from '../lib/format'
import type {
  ArtistDetail as ArtistPayload,
  ArtistDiscography,
  DiscographyExcluded,
  LazyFetchState,
} from '../lib/types'
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

/**
 * How often the page asks again while Spotify is being walked.
 *
 * Three seconds rather than the album page's two, because the answer is further
 * away. An album's track list is one Spotify request; a discography is a
 * paginated walk of up to forty of them, made one after another against a
 * limiter the whole instance shares. Asking more often cannot make that walk
 * finish sooner — every `pending` response for one artist is byte-identical, so
 * a tighter interval buys nothing but load — and three seconds is still short
 * enough that a list which is genuinely coming lands while somebody is looking
 * at the page.
 */
const DISCOGRAPHY_POLL_MS = 3000

/**
 * How long this page keeps asking before it stops.
 *
 * The cap is not an optimisation, it is the only thing that ends the poll.
 * `pending` is unbounded server-side: a claim against `artist_album_fetches`
 * that errors records nothing, so the very next request re-enters the same
 * branch, and every `pending` response for one artist is identical however long
 * the state has held. Without this, a read-only replica or a full tablespace
 * turns every open artist tab into a request every three seconds, for ever.
 *
 * Three minutes is read off the server's own numbers rather than picked: one
 * artist's whole walk is bounded at two minutes (`artistalbums.fetchTimeout`)
 * and a lease stranded by a killed process expires after three
 * (`artistalbums.leaseTTL`), so any walk that is going to resolve has resolved
 * by here. `docs/api.md` states the same three minutes as this client's
 * behaviour.
 */
const DISCOGRAPHY_POLL_CAP_MS = 180_000

/**
 * The cap above, in words, for the one line of copy that has to say how long it
 * waited. Kept adjacent so changing the number cannot silently leave the
 * sentence behind.
 */
const DISCOGRAPHY_POLL_CAP_LABEL = 'three minutes'

/**
 * How old a recorded poll start may be before it is treated as a different
 * visit rather than a continuation of this one. Twice the cap, for the reason
 * given on the album page's equivalent.
 */
const DISCOGRAPHY_POLL_WINDOW_MS = 2 * DISCOGRAPHY_POLL_CAP_MS

/**
 * Key prefix for when this tab first saw `pending` for an artist.
 *
 * Its own prefix, not the album page's: the two id spaces are disjoint in
 * practice, but the prefix is what guarantees one panel's stuck walk cannot make
 * the other report having given up. Exported for the test that reloads a page
 * near the cap.
 */
export const DISCOGRAPHY_POLL_START_KEY = 'encore.discography-poll-start.'

/** The next poll delay, or `false` to stop. Exported so it can be tested without a timer. */
export function discographyPollInterval(
  state: LazyFetchState | undefined,
  gaveUp = false,
): number | false {
  return lazyPollInterval(state, gaveUp, DISCOGRAPHY_POLL_MS)
}

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

          {/*
            Keyed by artist so that walking from one to another cannot carry the
            previous one's poll — its own request, its own cap.
          */}
          <DiscographyPanel key={artist.id} artistId={artist.id} timeZone={timeZone} />

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

/**
 * "1 single, 1 compilation, 1 appearance and 1 other release", omitting empty
 * buckets entirely.
 *
 * Returns null when nothing was excluded, so the caller renders no sentence
 * rather than one that lists nothing. `other` is any album_group Spotify sends
 * that is none of the four it documents; it is zero today and named rather than
 * dropped so this sentence still accounts for every release the response
 * counted.
 */
function excludedList(excluded: DiscographyExcluded): string | null {
  const parts: string[] = []
  if (excluded.singles > 0) parts.push(formatPlural(excluded.singles, 'single'))
  if (excluded.compilations > 0) parts.push(formatPlural(excluded.compilations, 'compilation'))
  if (excluded.appearsOn > 0) parts.push(formatPlural(excluded.appearsOn, 'appearance'))
  if (excluded.other > 0) parts.push(formatPlural(excluded.other, 'other release'))
  const last = parts.pop()
  if (last === undefined) return null
  if (parts.length === 0) return last
  return `${parts.join(', ')} and ${last}`
}

/**
 * The panel's own description when there is a list to describe.
 *
 * Both the verb and the denominator bend to the numbers. "1 of the 11 albums …
 * have no plays" disagrees with itself, and one unplayed album is among the
 * commonest things this panel reports; "1 of the 1 album" is not a sentence
 * anybody writes, so a single-album discography drops the ratio altogether.
 *
 * Every form carries the exclusion clause. It is the difference between a true
 * sentence and one that describes an artist with fifty releases as an artist
 * with eleven, and it must survive every edit to the numbers around it.
 */
function discographySummary(missing: number, listed: number): string {
  const tail =
    'no plays in your history, all time. Singles, compilations and appearances are not counted.'
  if (listed === 1) {
    return `The only album Spotify lists for this artist has ${tail}`
  }
  return `${formatCount(missing)} of the ${formatPlural(listed, 'album')} Spotify lists for this artist ${
    missing === 1 ? 'has' : 'have'
  } ${tail}`
}

/**
 * How much of this artist's own catalogue you have played.
 *
 * Everything else on this page is computed from listening Encore already holds.
 * This is not: it needs Spotify's own list of what the artist released, which
 * Encore reads the first time somebody opens this page and then keeps. So an
 * empty list here means one of several different things, and saying which is the
 * whole job:
 *
 *   pending     — Encore has not been told what they released yet
 *   unavailable — Encore asked and could not find out
 *   disabled    — this instance does not ask, because its operator said not to
 *   ready       — Encore knows, and either you have played something from all of
 *                 them, or there is nothing here it counts
 *
 * plus one that belongs to this page rather than to the server: `pending` that
 * has outlasted the poll's cap, which is neither a refusal nor still in
 * progress.
 *
 * The cap lives here rather than on the page because it belongs to one artist.
 * The page mounts this keyed by artist id, so nothing about one artist's stuck
 * walk can follow a reader to the next.
 */
function DiscographyPanel({
  artistId,
  timeZone,
}: {
  artistId: string
  timeZone: string
}): ReactElement {
  const [gaveUp, setGaveUp] = useState(false)
  const query = useQuery({
    queryKey: qk.artistDiscography(artistId),
    queryFn: ({ signal }) =>
      api.get<ArtistDiscography>(
        `/artists/${encodeURIComponent(artistId)}/discography`,
        undefined,
        signal,
      ),
    enabled: artistId !== '',
    refetchInterval: (cached) => discographyPollInterval(cached.state.data?.state, gaveUp),
  })
  const data = query.data
  const state = data?.state

  // Stopping the requests is only half of it: the panel has to say it has
  // stopped, and once the poll ends no further response is coming to re-render
  // it. Hence a timer for the moment the cap passes, sized from the persisted
  // start so a reload resumes the same window rather than opening a new one.
  useEffect(() => {
    const key = pollStartKey(DISCOGRAPHY_POLL_START_KEY, artistId)
    if (state !== 'pending') {
      // A settled answer closes the window, so an artist that returns to
      // `pending` much later is given their own full three minutes.
      if (state) clearPollStart(key)
      return
    }
    const remaining =
      pollStartedAt(key, Date.now(), DISCOGRAPHY_POLL_WINDOW_MS) +
      DISCOGRAPHY_POLL_CAP_MS -
      Date.now()
    const timer = window.setTimeout(() => setGaveUp(true), Math.max(remaining, 0))
    return () => {
      window.clearTimeout(timer)
    }
  }, [artistId, state])

  return (
    <Panel
      title="Albums you have never played"
      description={
        // The count is stated only when there is one. With nothing missing it
        // becomes "0 of the 11 albums … have no plays", which is the same fact
        // the body states below it, phrased as a double negative and said twice.
        data?.state === 'ready' && data.missing.length > 0
          ? discographySummary(data.missing.length, data.coverage.total)
          : // This one line sits above seven different bodies, so it may assert
            // nothing that is not true of all seven. Anything about where the
            // list came from is false on "disabled", where no read has ever
            // happened and none ever will, and premature on "pending". So it
            // says only what the panel is for, carries the all-time qualifier
            // the count line opposite it carries, and states the exclusion —
            // which is a rule about what this panel counts and is therefore true
            // even where nothing has been counted.
            "Which of this artist's albums have no plays in your history, all time. Singles, compilations and appearances are not counted."
      }
      padded={false}
    >
      <MissingAlbums
        data={data}
        isPending={query.isPending}
        failed={query.isError}
        gaveUp={gaveUp}
        timeZone={timeZone}
      />
    </Panel>
  )
}

/**
 * The body under that description: which of the artist's albums have no plays.
 *
 * `disabled` and `unavailable` are kept apart deliberately: one is somebody here
 * choosing not to ask and the other is Spotify not answering, and telling a
 * person the second when the first is true blames a third party for a local
 * decision. A discography cached before the switch was turned off still arrives
 * as `ready` — turning off fetching does not hide what is already on disk — and
 * the date it was read is rendered on every `ready` state, which is what keeps a
 * list that will never refresh from reading as though it were current.
 *
 * There is no reconciliation line, unlike the album panel. That panel prints
 * both totals when Spotify's listing and `albums.total_tracks` disagree; here
 * there is no second number, because nothing Encore stores counts an artist's
 * releases — that absence is the whole premise of the feature.
 */
function MissingAlbums({
  data,
  isPending,
  failed,
  gaveUp,
  timeZone,
}: {
  data: ArtistDiscography | undefined
  isPending: boolean
  failed: boolean
  gaveUp: boolean
  timeZone: string
}): ReactElement {
  // The state is checked before the list is, deliberately. Branching on
  // `missing.length` first would render "you have played something from every
  // album" for an artist Encore has not even asked about yet.
  if (data?.state === 'disabled') {
    return (
      <EmptyState
        title="Artist discographies are turned off"
        description="This instance does not ask Spotify what an artist has released, so Encore cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. An administrator can turn this on with ENCORE_ARTIST_ALBUMS_ENABLED."
      />
    )
  }
  if (failed || data?.state === 'unavailable') {
    return (
      <EmptyState
        title="This artist's discography could not be read"
        description="Encore could not get the list of what this artist has released from Spotify, so it cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later."
      />
    )
  }
  // Having run out of patience is not the same fact as having been refused, and
  // it gets its own words rather than borrowing the ones above. What is known
  // here is only that `pending` held for the whole window; what caused it is
  // very likely local — a claim against artist_album_fetches that errors logs a
  // warning and persists nothing, which a read-only replica or a full tablespace
  // will do all day — so naming Spotify as the party that would not answer is
  // the "disabled" mistake arriving through another door. The sentence also
  // makes no promise to retry, because this page cannot keep one: the recorded
  // window outlives the visit.
  if (gaveUp && data?.state === 'pending') {
    return (
      <EmptyState
        title="No discography for this artist yet"
        description={`Encore waited ${DISCOGRAPHY_POLL_CAP_LABEL} for this artist's discography and has stopped for now; it may still arrive — reopen this page to check. Every other figure on this page comes from your own history and is unaffected.`}
      />
    )
  }
  // A walk in progress (`pending`, confirmed by the server) and a request this
  // panel has not had answered yet (`isPending`) are different facts. Sharing one
  // body would have an instance with fetching turned off claim "Asking Spotify"
  // for the whole round trip and then contradict itself — on the instance whose
  // operator explicitly asked Encore not to talk to Spotify.
  if (isPending && !data) {
    return (
      <div className="px-4 py-3" role="status" aria-live="polite" aria-busy="true">
        <span className="sr-only">Loading this artist&rsquo;s discography</span>
        <SkeletonText lines={2} className="max-w-md" />
      </div>
    )
  }
  if (!data || data.state === 'pending') {
    return (
      <EmptyState
        title="Asking Spotify what this artist has released"
        description="Encore reads it once and keeps it, so this step is skipped on most visits. The list appears here on its own."
      />
    )
  }

  const listed = data.coverage.total
  const excluded = excludedList(data.excluded)
  // Rendered on every `ready` state, not only when something is missing. On an
  // instance with fetching turned off this list will never change again, and a
  // list with no date on it reads as though it were current.
  const readOn = data.fetchedAt
    ? `Discography read from Spotify on ${formatDate(data.fetchedAt, timeZone)}.`
    : null
  // What "played" means here, said wherever a number is shown. "4 of 11 albums"
  // otherwise reads as four records heard end to end. The second sentence
  // pre-empts the contradiction a reader would find between this panel and the
  // Top albums panel on the same screen, which ranks by play and is not
  // restricted to the album group; it is also the nearest this page comes to
  // disclosing market relinking, where a play recorded against a relinked album
  // id does not match the canonical id this panel counts against. Encore does
  // not guess at those equivalences — see docs/api.md.
  const countsAsPlayed =
    'An album counts as played when you have played any track from it. Albums you played that Spotify does not list under this artist are not counted here.'

  if (listed === 0) {
    // Not a failure and not "you have played everything": Spotify answered, and
    // the answer is that nothing it lists for this artist is something this panel
    // counts. It has no counterpart on the album page, where a record with no
    // tracks does not exist and an empty listing is recorded as a failure.
    return (
      <EmptyState
        title="Spotify lists no albums for this artist"
        description={
          <>
            {'Nothing Spotify lists for them is an album, and an album is all this panel counts.'}
            {excluded ? (
              // No "also": nothing else was listed, so there is nothing for this
              // to be in addition to. And no "does not count": the sentence
              // directly above has just said so.
              <span className="mt-1.5 block">{`Spotify lists ${excluded} for this artist.`}</span>
            ) : null}
            {readOn ? <span className="mt-1.5 block text-xs text-ink-faint">{readOn}</span> : null}
          </>
        }
      />
    )
  }

  const excludedLine = excluded
    ? `Spotify also lists ${excluded} for this artist, which this panel does not count.`
    : null

  if (data.missing.length === 0) {
    return (
      <EmptyState
        icon="album"
        // Not "you have played every album": coverage counts an album with any
        // play, and the shorter sentence claims eleven records heard end to end.
        title="You have played something from every album by this artist"
        description={
          <>
            {`Spotify lists ${formatPlural(listed, 'album')} for this artist.`}
            {excludedLine ? <span className="mt-1.5 block">{excludedLine}</span> : null}
            <span className="mt-1.5 block">{countsAsPlayed}</span>
            {readOn ? <span className="mt-1.5 block text-xs text-ink-faint">{readOn}</span> : null}
          </>
        }
      />
    )
  }

  return (
    <div className="px-4 py-3">
      {/*
        `missing` has no ceiling. The album panel's equivalent was capped by how
        many tracks a record actually has; this one is capped only by how many
        albums Spotify lists for the artist, which is hundreds for a prolific
        one. So the rows scroll in their own box and the three sentences that
        qualify the number stay outside it — otherwise the exclusion disclosure
        would still be in the DOM and several hundred rows below the screen,
        which is the same as not saying it.
      */}
      <ul className="max-h-96 divide-y divide-seam overflow-y-auto">
        {data.missing.map((album) => (
          <li key={album.id} className="flex items-baseline gap-3 py-2 text-sm">
            {/* No link. Most of these are records nobody has played, so they are
                not in the catalogue and /albums/{id} would 404 on almost all of
                them. */}
            <span className="min-w-0 flex-1 truncate text-ink">{album.name}</span>
            <span className="tabular shrink-0 text-ink-faint">
              {formatRelease(album.releaseDate, album.releasePrecision)}
            </span>
          </li>
        ))}
      </ul>
      {excludedLine ? <p className="mt-3 text-sm text-ink-muted">{excludedLine}</p> : null}
      <p className="mt-2 text-sm text-ink-muted">{countsAsPlayed}</p>
      {readOn ? <p className="mt-2 text-xs text-ink-faint">{readOn}</p> : null}
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
      <div className="panel h-40" />
    </div>
  )
}
