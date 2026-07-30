/**
 * The library: what has been saved and followed on Spotify, crossed against
 * what has actually been played.
 *
 * Spotify's library is enumerated by a background worker at most once a day,
 * never on demand, so every figure here is only as fresh as that last run —
 * and until it has run at all, `syncedAt` is null rather than a zero
 * timestamp. That null means "nothing has been read yet", which is a
 * different fact from "read, and found nothing", and the two must never
 * render the same way.
 *
 * The three lists below are scoped in three different ways, and the page
 * says so rather than leaving it implicit: "Saved but never played" is all
 * time, because scoping "never" to the selected range would just list
 * whatever was not played in the last thirty days. "Played but never saved"
 * and "Dormant follows" both follow the range picker, like every other
 * ranked list in the app. The three snapshot counts and the read time follow
 * neither — they describe the last enumeration itself, not a window of
 * listening.
 *
 * Reading this page at all needs `user-library-read` and `user-follow-read`,
 * two scopes an account that connected before Encore asked for them will not
 * have granted. That state is read straight off the session — the same
 * `missingScopes` the reconsent banner already uses — rather than inferred
 * from a failed request, because a 403 from the one endpoint this page calls
 * would otherwise have to be reverse-engineered back into "share your
 * library", and the session already knows.
 */

import type { ReactElement, ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { useSession } from '../lib/session'
import { EMPTY, formatCount, formatDate, formatDateTime, formatRelative } from '../lib/format'
import type {
  LibraryDormantArtist,
  LibraryPlayedTrack,
  LibrarySavedTrack,
  LibraryStatsResponse,
} from '../lib/types'
import {
  EmptyState,
  ErrorState,
  Icon,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Panel,
  RangeLink,
  RangePicker,
  SkeletonLedger,
  Stat,
  StatGrid,
  buttonClass,
} from '../components/ui'
import type { ArtworkKind } from './top/TopList'
import { Artwork, EntityLedger } from './top/TopList'

/**
 * The two scopes this page needs. Either one missing means the library has
 * not been shared, regardless of what else the account granted.
 */
const REQUIRED_SCOPES = ['user-library-read', 'user-follow-read']

export default function Library(): ReactElement {
  const { spotify } = useSession()
  const { range, label, timeZone } = useRange()
  const missingScopes = spotify?.missingScopes ?? []
  const scopeBlocked = missingScopes.some((scope) => REQUIRED_SCOPES.includes(scope))

  const query = useQuery({
    queryKey: qk.library(range),
    queryFn: ({ signal }) =>
      api.get<LibraryStatsResponse>('/stats/library', { from: range.from, to: range.to }, signal),
    // No point asking for a library the session already knows is not shared —
    // the request would just be a 403 the page would have to interpret back
    // into the same banner it can show immediately from `missingScopes`.
    enabled: !scopeBlocked,
  })

  const data = query.data
  const empty =
    data !== undefined &&
    data.syncedAt !== null &&
    data.savedTracks === 0 &&
    data.savedAlbums === 0 &&
    data.followedArtists === 0

  // Deliberately paraphrased rather than a repeat of the visible copy below:
  // an assistive-technology user hears this once from the live region and
  // would otherwise hear the identical sentence again on landing on the
  // visible text it announces.
  const status = scopeBlocked
    ? 'Your library has not been shared with Encore.'
    : query.isPending
      ? 'Loading your library.'
      : query.isError
        ? 'Your library could not be loaded.'
        : !data
          ? ''
          : data.syncedAt === null
            ? 'Your library has not been read from Spotify yet.'
            : empty
              ? 'Your saved library is empty.'
              : `Your library statistics have loaded, last read ${formatRelative(data.syncedAt)}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title="Library"
        description="What you've saved, played and followed on Spotify."
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      {scopeBlocked ? (
        <ScopeGate />
      ) : query.isPending ? (
        <LoadingBody />
      ) : query.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title="Your library could not be loaded"
            onRetry={() => {
              void query.refetch()
            }}
          />
        </Panel>
      ) : !data ? null : data.syncedAt === null ? (
        <Panel padded={false}>
          <EmptyState
            icon="library"
            title="Encore has not read your Spotify library yet"
            description="It checks once a day; this page will fill in after the next run."
          />
        </Panel>
      ) : empty ? (
        <Panel padded={false}>
          <EmptyState
            icon="library"
            title="You have not saved anything on Spotify"
            description="Save a track or album, or follow an artist, and Encore will pick it up at the next daily read."
          />
        </Panel>
      ) : (
        <LibraryBody
          data={data}
          syncedAt={data.syncedAt}
          label={label}
          timeZone={timeZone}
          busy={query.isFetching}
        />
      )}
    </div>
  )
}

/** The library has not been shared: nothing here is an empty result, it is a permission this account never granted. */
function ScopeGate(): ReactElement {
  return (
    <Panel padded={false}>
      <EmptyState
        icon="library"
        title="Your Spotify library isn't shared with Encore"
        description="Seeing what you've saved, played and followed needs read access this account has not granted. Reconnecting does not let Encore change anything on your Spotify account."
        action={
          <a
            // A full navigation, not a fetch: the server answers with a
            // redirect to Spotify's own authorisation page.
            href="/api/auth/spotify/relink"
            className={buttonClass('primary')}
          >
            <Icon name="refresh" />
            Reconnect Spotify
          </a>
        }
      />
    </Panel>
  )
}

/** The page's shape while the one request is in flight, so nothing jumps when it lands. */
function LoadingBody(): ReactElement {
  return (
    <div className="space-y-4" role="status" aria-live="polite" aria-busy="true">
      <span className="sr-only">Loading your library</span>
      <div className="panel h-28" />
      <Panel padded={false}>
        <SkeletonLedger rows={6} columns={3} />
      </Panel>
      <Panel padded={false}>
        <SkeletonLedger rows={6} columns={5} />
      </Panel>
      <Panel padded={false}>
        <SkeletonLedger rows={6} columns={3} />
      </Panel>
    </div>
  )
}

interface LibraryBodyProps {
  data: LibraryStatsResponse
  /** Narrowed to non-null by the caller, so the snapshot line needs no further check. */
  syncedAt: string
  label: string
  timeZone: string
  busy: boolean
}

/** The synced, non-empty page: the snapshot, and the three cross-referenced lists. */
function LibraryBody({ data, syncedAt, label, timeZone, busy }: LibraryBodyProps): ReactElement {
  return (
    <div className={busy ? 'space-y-4 opacity-60 transition-opacity' : 'space-y-4 transition-opacity'}>
      <Panel title="Snapshot" description={`Last read from Spotify ${formatRelative(syncedAt)}.`}>
        <StatGrid columns={3}>
          <Stat label="Saved tracks" value={formatCount(data.savedTracks)} />
          <Stat label="Saved albums" value={formatCount(data.savedAlbums)} />
          <Stat label="Followed artists" value={formatCount(data.followedArtists)} />
        </StatGrid>
      </Panel>

      <Panel
        title="Saved but never played"
        description="Most recently saved first · all time"
        padded={false}
      >
        {data.savedNeverPlayed.length === 0 ? (
          <EmptyState
            icon="track"
            title={
              data.savedTracks === 0
                ? 'You have not saved any tracks on Spotify'
                : 'Every track you have saved has at least one play'
            }
          />
        ) : (
          <SavedNeverPlayedTable rows={data.savedNeverPlayed} timeZone={timeZone} />
        )}
      </Panel>

      <Panel
        title="Played but never saved"
        description={`Tracks you played that are not in your saved library, most played first — ${label.toLowerCase()}.`}
        padded={false}
      >
        {data.playedNeverSaved.length === 0 ? (
          <EmptyState
            icon="track"
            title={`Every track you played is already saved to your library — ${label.toLowerCase()}`}
          />
        ) : (
          <PlayedNeverSavedTable rows={data.playedNeverSaved} />
        )}
      </Panel>

      <Panel
        title="Dormant follows"
        description={`Artists you follow but did not play, most dormant first — ${label.toLowerCase()}.`}
        padded={false}
      >
        {data.dormantFollows.length === 0 ? (
          <EmptyState
            icon="artist"
            title={
              data.followedArtists === 0
                ? 'You are not following any artists on Spotify'
                : `Every artist you follow was played — ${label.toLowerCase()}`
            }
          />
        ) : (
          <DormantFollowsTable rows={data.dormantFollows} timeZone={timeZone} />
        )}
      </Panel>
    </div>
  )
}

// --- the three tables --------------------------------------------------

/**
 * The shared shell the two bespoke tables below sit in: artwork, a name and
 * an optional second line, then one extra column. `EntityLedger` does not
 * fit either list — it always draws Plays and Time, and neither list here
 * has both — so this is the same visual idiom (`Ledger`, `Artwork`, a
 * row-header naming the entity) built for the one column each actually has.
 */
interface LibraryTableRow {
  key: string
  to: string
  name: string
  meta?: string
  imageUrl: string
  /** The one figure this list adds beyond a name: an added date, a last play. */
  extra: ReactNode
  extraTitle?: string
}

function LibraryTable({
  caption,
  column,
  extraColumn,
  kind,
  rows,
}: {
  caption: string
  column: string
  extraColumn: string
  kind: ArtworkKind
  rows: LibraryTableRow[]
}): ReactElement {
  return (
    <Ledger caption={caption}>
      <LedgerHead>
        <LedgerRow>
          <LedgerHeaderCell className="w-12">
            <span className="sr-only">Artwork</span>
          </LedgerHeaderCell>
          <LedgerHeaderCell>{column}</LedgerHeaderCell>
          <LedgerHeaderCell>{extraColumn}</LedgerHeaderCell>
        </LedgerRow>
      </LedgerHead>
      <LedgerBody>
        {rows.map((row) => (
          <LedgerRow key={row.key}>
            <LedgerCell>
              <Artwork src={row.imageUrl} kind={kind} size={32} />
            </LedgerCell>
            <LedgerRowHeader className="text-sm font-normal tracking-normal normal-case">
              <RangeLink
                to={row.to}
                className="block max-w-[10rem] truncate text-ink hover:text-lamp sm:max-w-[22rem]"
              >
                {row.name}
              </RangeLink>
              {row.meta ? (
                <span className="block max-w-[10rem] truncate text-xs text-ink-muted sm:max-w-[22rem]">
                  {row.meta}
                </span>
              ) : null}
            </LedgerRowHeader>
            <LedgerCell className="whitespace-nowrap" title={row.extraTitle}>
              {row.extra}
            </LedgerCell>
          </LedgerRow>
        ))}
      </LedgerBody>
    </Ledger>
  )
}

function SavedNeverPlayedTable({
  rows,
  timeZone,
}: {
  rows: LibrarySavedTrack[]
  timeZone: string
}): ReactElement {
  return (
    <LibraryTable
      caption="Tracks you have saved on Spotify that nothing in your history shows you playing"
      column="Track"
      extraColumn="Added"
      kind="track"
      rows={rows.map((row) => ({
        key: row.entity.id,
        to: `/tracks/${row.entity.id}`,
        name: row.entity.name,
        meta: row.entity.artists.map((artist) => artist.name).join(', '),
        imageUrl: row.entity.album?.imageUrl ?? '',
        extra: row.addedAt ? formatDate(row.addedAt, timeZone) : EMPTY,
        extraTitle: row.addedAt ? formatDateTime(row.addedAt, timeZone) : undefined,
      }))}
    />
  )
}

function DormantFollowsTable({
  rows,
  timeZone,
}: {
  rows: LibraryDormantArtist[]
  timeZone: string
}): ReactElement {
  return (
    <LibraryTable
      caption="Artists you follow with no play in this range, most dormant first"
      column="Artist"
      extraColumn="Last played"
      kind="artist"
      rows={rows.map((row) => ({
        key: row.entity.id,
        to: `/artists/${row.entity.id}`,
        name: row.entity.name,
        imageUrl: row.entity.imageUrl,
        extra: row.lastPlayedAt ? formatRelative(row.lastPlayedAt) : 'Never played',
        extraTitle: row.lastPlayedAt ? formatDateTime(row.lastPlayedAt, timeZone) : undefined,
      }))}
    />
  )
}

/**
 * Ranked by plays, exactly like a top list — the one list on this page where
 * `EntityLedger` fits without alteration, since "most played first" is a
 * genuine ranking rather than a chronological order borrowing a rank column
 * it does not mean.
 */
function PlayedNeverSavedTable({ rows }: { rows: LibraryPlayedTrack[] }): ReactElement {
  return (
    <EntityLedger
      caption="Tracks you played in this range that you have not saved, ranked by plays"
      column="Track"
      kind="track"
      rows={rows.map((row, index) => ({
        key: row.entity.id,
        to: `/tracks/${row.entity.id}`,
        name: row.entity.name,
        imageUrl: row.entity.album?.imageUrl ?? '',
        meta: row.entity.artists.map((artist) => artist.name).join(', '),
        plays: row.plays,
        msPlayed: row.msPlayed,
        rank: index + 1,
      }))}
    />
  )
}
