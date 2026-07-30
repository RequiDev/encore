/**
 * Top diff: Spotify's own top ranking against Encore's, for the same kind
 * and window.
 *
 * The reason this page exists is also the reason it opens with a disclaimer
 * rather than a table: Spotify's own ranking is opaque. It calls it
 * "calculated affinity", it is not a play count, and it draws on listening
 * this instance has never seen at all. Without saying that plainly, every
 * place the two columns disagree reads as an Encore bug, and the whole
 * feature becomes a support burden instead of an interesting comparison.
 *
 * Everything else follows `Library.tsx`'s lead, because it solved the same
 * shape of problem: a missing scope, a set that has never been captured, and
 * a genuinely empty comparison are three different facts, and `capturedAt`
 * being nullable is what keeps the second and third from collapsing into the
 * same screen.
 *
 * There is deliberately no date-range picker here. The window comes from
 * Spotify's own `short_term` / `medium_term` / `long_term` — see
 * `stats.topDiffWindow` on the server — not from the app's `from`/`to`, and
 * the page says so rather than leaving the missing control unexplained.
 *
 * A blacklisted artist (or a track credited to one) is removed from *both*
 * sides before ranking, with the surviving ranks closed up — the same rule
 * `internal/stats/topdiff.go` documents at length. That is not repeated as
 * on-page copy: no other ranked list in the app (top tracks, top artists,
 * the library page) restates the blacklist rule for the viewer either, and
 * doing it only here would read as a warning about this page specifically
 * rather than the instance-wide behaviour it actually is.
 */

import type { ReactElement } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useSession } from '../lib/session'
import { EMPTY, formatCount, formatPlural, formatRelative, rankChange } from '../lib/format'
import type {
  ArtistRef,
  SpotifyTimeRange,
  TopDiffEntry,
  TopDiffKind,
  TopDiffResponse,
  TrackRef,
} from '../lib/types'
import {
  EmptyState,
  ErrorState,
  Field,
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
  Select,
  SkeletonLedger,
  buttonClass,
} from '../components/ui'
import type { ArtworkKind } from './top/TopList'
import { Artwork } from './top/TopList'

/** Reading Spotify's own top ranking needs this scope alone. */
const REQUIRED_SCOPE = 'user-top-read'

type TopDiffEntity = TrackRef | ArtistRef

/** Duck-typed the same way `top/TopList.tsx` tells a `TrackRef` from an `ArtistRef` — a track is the only one of the two with credited artists of its own. */
function isTrackEntity(entity: TopDiffEntity): entity is TrackRef {
  return 'artists' in entity
}

function imageOf(entity: TopDiffEntity): string {
  return isTrackEntity(entity) ? (entity.album?.imageUrl ?? '') : entity.imageUrl
}

function metaOf(entity: TopDiffEntity): string {
  return isTrackEntity(entity) ? entity.artists.map((artist) => artist.name).join(', ') : ''
}

function pathOf(kind: TopDiffKind, id: string): string {
  return kind === 'track' ? `/tracks/${id}` : `/artists/${id}`
}

interface KindCopy {
  nouns: string
  column: string
  kind: ArtworkKind
}

const KIND_COPY: Record<TopDiffKind, KindCopy> = {
  artist: { nouns: 'artists', column: 'Artist', kind: 'artist' },
  track: { nouns: 'tracks', column: 'Track', kind: 'track' },
}

/** Spotify's own approximate wording for each window (see `stats.topDiffWindow`), as the em-dash appositive every range label on this page reuses rather than inventing a new construction. */
const RANGE_LABEL: Record<SpotifyTimeRange, string> = {
  short_term: 'last 4 weeks',
  medium_term: 'last 6 months',
  long_term: 'last 12 months',
}

const RANGE_OPTIONS: { value: SpotifyTimeRange; label: string }[] = [
  { value: 'short_term', label: 'Last 4 weeks' },
  { value: 'medium_term', label: 'Last 6 months' },
  { value: 'long_term', label: 'Last 12 months' },
]

function readKind(value: string | null): TopDiffKind {
  return value === 'track' ? 'track' : 'artist'
}

function readRange(value: string | null): SpotifyTimeRange {
  return value === 'medium_term' || value === 'long_term' ? value : 'short_term'
}

export default function TopDiff(): ReactElement {
  const { spotify } = useSession()
  const missingScopes = spotify?.missingScopes ?? []
  const scopeBlocked = missingScopes.includes(REQUIRED_SCOPE)

  const [params, setParams] = useSearchParams()
  const kind = readKind(params.get('kind'))
  const range = readRange(params.get('range'))
  const copy = KIND_COPY[kind]
  const noun = copy.nouns.slice(0, -1)

  const query = useQuery({
    queryKey: qk.topDiff(kind, range),
    queryFn: ({ signal }) =>
      api.get<TopDiffResponse<TopDiffEntity>>('/stats/top-diff', { kind, range }, signal),
    // No point asking for a comparison the session already knows is not
    // shared — the request would just be a 403 the page would have to
    // interpret back into the same banner it can show immediately from
    // `missingScopes`, exactly as Library.tsx reasons about the same call.
    enabled: !scopeBlocked,
  })

  const setKind = (next: TopDiffKind): void => {
    setParams((current) => {
      const updated = new URLSearchParams(current)
      updated.set('kind', next)
      return updated
    })
  }
  const setRange = (next: SpotifyTimeRange): void => {
    setParams((current) => {
      const updated = new URLSearchParams(current)
      updated.set('range', next)
      return updated
    })
  }

  const data = query.data
  const entries = data?.entries ?? []
  const neverCaptured = data !== undefined && data.capturedAt === null

  // Deliberately paraphrased rather than a repeat of the visible copy below:
  // an assistive-technology user hears this once from the live region and
  // would otherwise hear the identical sentence again on landing on the
  // visible text it announces.
  const status = scopeBlocked
    ? "Spotify's own top ranking has not been shared with Encore."
    : query.isPending
      ? `Loading the top ${copy.nouns} comparison.`
      : query.isError
        ? `The top ${copy.nouns} comparison could not be loaded.`
        : neverCaptured
          ? `Spotify's top ${copy.nouns} ranking has not been captured yet.`
          : entries.length === 0
            ? `Spotify reported no top ${copy.nouns} for this window.`
            : `Comparing ${formatPlural(entries.length, noun)} between Spotify's ranking and Encore's, ${RANGE_LABEL[range]}.`

  const picker = (
    <div className="flex flex-wrap items-center gap-2">
      <Field label="Kind" labelHidden className="w-28">
        <Select value={kind} onChange={(event) => setKind(event.target.value as TopDiffKind)}>
          <option value="artist">Artists</option>
          <option value="track">Tracks</option>
        </Select>
      </Field>
      <Field label="Spotify's time range" labelHidden className="w-40">
        <Select value={range} onChange={(event) => setRange(event.target.value as SpotifyTimeRange)}>
          {RANGE_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </Select>
      </Field>
    </div>
  )

  return (
    <div className="space-y-4">
      <PageHeader
        title="Top diff"
        description={`How Spotify's own top ${copy.nouns} ranking compares with Encore's.`}
        actions={picker}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <Panel title="Before you compare">
        <div className="space-y-2 text-sm text-ink-muted">
          <p>
            Spotify calls this "calculated affinity". It isn't a play count, its time ranges are
            approximate, and it covers your listening everywhere — including before this instance
            existed. Disagreeing with Encore's ranking is normal.
          </p>
          <p>
            There's no date range picker on this page. The window above is Spotify's own —{' '}
            {RANGE_LABEL[range]} — not the range you'd pick elsewhere: comparing against a
            different window would make the two sides answers to different questions rather than
            a real disagreement about the same one.
          </p>
        </div>
      </Panel>

      {scopeBlocked ? (
        <ScopeGate />
      ) : query.isPending ? (
        <Panel padded={false}>
          <SkeletonLedger rows={10} columns={5} />
        </Panel>
      ) : query.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title={`The top ${copy.nouns} comparison could not be loaded`}
            onRetry={() => {
              void query.refetch()
            }}
          />
        </Panel>
      ) : !data ? null : data.capturedAt === null ? (
        <Panel padded={false}>
          <EmptyState
            icon={copy.kind}
            title={`Encore has not captured Spotify's top ${copy.nouns} ranking yet`}
            description="Checked once a day, alongside your library sync — this page fills in the next time that sync completes successfully."
          />
        </Panel>
      ) : entries.length === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon={copy.kind}
            title={`Spotify reported no top ${copy.nouns} for this window`}
          />
        </Panel>
      ) : (
        <Panel
          title="Comparison"
          description={`Last captured ${formatRelative(data.capturedAt)} — refreshed once a day, so this may be up to a day old.`}
          padded={false}
        >
          <DiffTable kind={kind} column={copy.column} entries={entries} />
        </Panel>
      )}
    </div>
  )
}

/** The comparison has not been shared: nothing here is an empty result, it is a permission this account never granted. */
function ScopeGate(): ReactElement {
  return (
    <Panel padded={false}>
      <EmptyState
        icon="compare"
        title="Spotify's own top ranking isn't shared with Encore"
        description="Comparing Spotify's ranking with Encore's needs read access this account hasn't granted. Reconnecting doesn't let Encore change anything on your Spotify account."
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

interface DiffTableProps {
  kind: TopDiffKind
  column: string
  entries: TopDiffEntry<TopDiffEntity>[]
}

function DiffTable({ kind, column, entries }: DiffTableProps): ReactElement {
  const artworkKind: ArtworkKind = kind === 'track' ? 'track' : 'artist'
  const nouns = KIND_COPY[kind].nouns

  return (
    <Ledger caption={`Spotify's and Encore's top ${nouns}, side by side`}>
      <LedgerHead>
        <LedgerRow>
          <LedgerHeaderCell numeric className="w-16">
            Spotify
          </LedgerHeaderCell>
          <LedgerHeaderCell className="w-12">
            <span className="sr-only">Artwork</span>
          </LedgerHeaderCell>
          <LedgerHeaderCell>{column}</LedgerHeaderCell>
          <LedgerHeaderCell numeric className="w-16">
            Encore
          </LedgerHeaderCell>
          {/*
            Spotify is the baseline, so a positive delta means Spotify ranks the
            entity higher than Encore does. The direction is inferable from the
            two rank columns beside it, but only by arithmetic — the title says
            it outright, because a reader who guesses the sign backwards reads
            every disagreement on the page inverted.
          */}
          <LedgerHeaderCell numeric className="w-16" title="How much higher Spotify ranks it than Encore does. Positive means Spotify ranks it higher.">
            Delta
          </LedgerHeaderCell>
          <LedgerHeaderCell numeric>Plays</LedgerHeaderCell>
        </LedgerRow>
      </LedgerHead>
      <LedgerBody>
        {entries.map((entry) => (
          <LedgerRow key={entry.entity.id}>
            <LedgerCell numeric>
              <RankCell
                rank={entry.spotifyRank}
                absentTitle="Not in Spotify's captured ranking for this window"
              />
            </LedgerCell>
            <LedgerCell>
              <Artwork src={imageOf(entry.entity)} kind={artworkKind} size={32} />
            </LedgerCell>
            <LedgerRowHeader className="text-sm font-normal tracking-normal normal-case">
              <Link
                to={pathOf(kind, entry.entity.id)}
                className="block max-w-[10rem] truncate text-ink hover:text-lamp sm:max-w-[22rem]"
              >
                {entry.entity.name}
              </Link>
              {metaOf(entry.entity) ? (
                <span className="block max-w-[10rem] truncate text-xs text-ink-muted sm:max-w-[22rem]">
                  {metaOf(entry.entity)}
                </span>
              ) : null}
            </LedgerRowHeader>
            <LedgerCell numeric>
              <RankCell
                rank={entry.encoreRank}
                absentTitle="Outside Encore's ranking for this window"
              />
            </LedgerCell>
            <LedgerCell numeric>
              <Delta spotifyRank={entry.spotifyRank} encoreRank={entry.encoreRank} />
            </LedgerCell>
            <LedgerCell numeric>{entry.encoreRank === null ? EMPTY : formatCount(entry.plays)}</LedgerCell>
          </LedgerRow>
        ))}
      </LedgerBody>
    </Ledger>
  )
}

/** A rank, or the dash that means "absent from this side" — never a zero, which would read as tied for last place rather than missing. */
function RankCell({ rank, absentTitle }: { rank: number | null; absentTitle: string }): ReactElement {
  if (rank === null) {
    return (
      <span className="text-ink-faint" title={absentTitle}>
        {EMPTY}
      </span>
    )
  }
  return <span className="tabular">{rank}</span>
}

/**
 * How Spotify's rank differs from Encore's rank for the same entity and
 * window — the delta the design doc asks for beside the two ranks
 * themselves, so "Spotify 2 / Encore 17" reads as the disagreement it is
 * without the reader doing the subtraction across the table.
 *
 * Reuses `rankChange` (`lib/format.ts`) for the arithmetic and its `+N` /
 * `-N` / `=` label, the same convention `Dashboard.tsx`'s period-over-period
 * `Movement` cell renders for rank change over time. There is no "new" case
 * here the way there is for `Movement`, though: `rankChange` reports one
 * when its second argument is null, but "new" means "absent in the
 * *previous period*", and there is no previous period in this comparison,
 * only two independent sides ranking the same one. An entity ranked on only
 * one side has nothing on the other side to take a delta against — treating
 * that as a rank change of however many places the present side's rank
 * happens to be would read as a real disagreement (e.g. "+17") when the
 * truth is "this side does not rank it at all" — so that case is handled
 * before `rankChange` is ever called, rendering the same dash `RankCell`
 * already uses for an absent rank rather than a number computed against
 * zero.
 *
 * Spotify is the baseline: the page frames the whole comparison as how
 * Spotify's own ranking compares with Encore's (see the page title and
 * description above), so a positive delta means Spotify ranks the entity
 * that many places higher — a better, lower position number — than Encore
 * does, matching the "up" `Movement` shows when a rank improves on its
 * reference.
 */
function Delta({
  spotifyRank,
  encoreRank,
}: {
  spotifyRank: number | null
  encoreRank: number | null
}): ReactElement {
  if (spotifyRank === null || encoreRank === null) {
    return (
      <span
        className="text-ink-faint"
        title="Ranked by only one side; there is nothing on the other side to take a delta against"
      >
        {EMPTY}
      </span>
    )
  }
  const change = rankChange(spotifyRank, encoreRank)
  if (change.direction === 'flat') {
    return <span className="text-ink-faint">{change.label}</span>
  }
  const description =
    change.direction === 'up'
      ? `Spotify ranks this ${formatPlural(change.places, 'place')} higher than Encore does`
      : `Spotify ranks this ${formatPlural(change.places, 'place')} lower than Encore does`
  return (
    <>
      <span aria-hidden="true">{change.label}</span>
      <span className="sr-only">{description}</span>
    </>
  )
}
