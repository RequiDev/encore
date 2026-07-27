/**
 * The three top lists, and the furniture the entity pages share with them.
 *
 * `TopList` is the whole of `/tracks`, `/artists` and `/albums`. Those three
 * pages differ only in their copy and in what the second line of a row says, so
 * they are one dense ledger with three configurations rather than three tables
 * that would drift apart by the second change to any of them.
 *
 * The artwork tile, the movement cell and the compact entity ledger live here
 * too. The detail pages need exactly those three things, and this is the module
 * the list and detail pages already have in common — a fourth file for a 40×40
 * image would be worse than a paragraph explaining why they share this one.
 *
 * Sorting is not a control. The API ranks by plays and offers no alternative, so
 * the panel says "ranked by plays" instead of drawing a column header that looks
 * clickable and is not.
 */

import type { ReactElement } from 'react'
import { useMemo, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { qk } from '../../lib/query'
import { useRange } from '../../lib/range'
import {
  EMPTY,
  formatCount,
  formatDate,
  formatDuration,
  formatMonth,
  formatPlural,
  formatRelative,
  rankChange,
} from '../../lib/format'
import type { AlbumRef, ArtistRef, EntityStats, Page, TopEntry, TrackRef } from '../../lib/types'
import {
  Button,
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  Icon,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRank,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Pagination,
  Panel,
  RangePicker,
  SkeletonLedger,
  Stat,
  StatGrid,
} from '../../components/ui'
import type { IconName } from '../../components/ui'
import { ChartCard, MetricToggle, TimelineChart } from '../../components/charts'
import type { TimelineMetric } from '../../components/charts'

/** Fifty rows is one screenful of ledger and one analytic query, not five. */
const PAGE_SIZE = 50

/**
 * `.ledger th` is written for column headings — small, uppercase, widely
 * tracked — and a row header is a `th` too. Every row-name cell puts the
 * ordinary text back, so a track called "Everything In Its Right Place" reads
 * as a title rather than as a legend.
 */
const ROW_HEADER = 'text-sm font-normal tracking-normal normal-case'

// --- artwork ---------------------------------------------------------------

export type ArtworkKind = 'track' | 'artist' | 'album'

const ARTWORK_ICON: Record<ArtworkKind, IconName> = {
  track: 'track',
  artist: 'artist',
  album: 'album',
}

export interface ArtworkProps {
  /** A Spotify CDN URL, or the empty string for a listen not yet enriched. */
  src: string
  kind: ArtworkKind
  /** Edge length in pixels. Set on the element, so nothing reflows when it lands. */
  size?: number
  /**
   * Usually empty: the name sits right beside the picture, and reading it twice
   * helps nobody. Pass one where the tile stands alone.
   */
  alt?: string
  className?: string
}

/**
 * A square of cover art, or a tile that looks like one.
 *
 * A missing image is the ordinary case rather than a failure — a listen imported
 * from an export carries no artwork until the catalogue lookup enriches it — so
 * the fallback is a deliberate tile with the right mark on it rather than a
 * broken-image glyph or a hole in the column.
 */
export function Artwork({ src, kind, size = 36, alt = '', className }: ArtworkProps): ReactElement {
  // Keyed by URL rather than a bare boolean: a paginated table reuses the same
  // component instance for a different row, and a stale failure would blank a
  // picture that is perfectly good.
  const [failedSrc, setFailedSrc] = useState<string | null>(null)
  const broken = src !== '' && failedSrc === src

  const shape = kind === 'artist' ? 'rounded-full' : 'rounded-[3px]'
  const box = ['shrink-0 border border-seam bg-panel-raised', shape, className]
    .filter(Boolean)
    .join(' ')
  const style = { width: size, height: size }

  if (src === '' || broken) {
    return (
      <span
        className={`${box} inline-flex items-center justify-center text-ink-faint`}
        style={style}
        role={alt ? 'img' : undefined}
        aria-label={alt || undefined}
        aria-hidden={alt ? undefined : true}
      >
        <Icon name={ARTWORK_ICON[kind]} size={Math.max(12, Math.round(size * 0.45))} />
      </span>
    )
  }

  return (
    <img
      src={src}
      alt={alt}
      width={size}
      height={size}
      loading="lazy"
      decoding="async"
      style={style}
      className={`${box} block object-cover`}
      onError={() => setFailedSrc(src)}
    />
  )
}

// --- movement --------------------------------------------------------------

export interface MovementProps {
  rank: number
  previousRank: number | null
}

/**
 * Rank movement against the equal-length period before this one. A null
 * previous rank means the entity was absent then, which is "new" rather than a
 * rise from infinity — and the sign alone means nothing read aloud, so every
 * cell carries a sentence too.
 */
export function Movement({ rank, previousRank }: MovementProps): ReactElement {
  const change = rankChange(rank, previousRank)
  if (change.direction === 'new') return <Chip tone="lamp">New</Chip>
  return (
    <>
      <span aria-hidden="true" className={change.direction === 'flat' ? 'text-ink-faint' : ''}>
        {change.label}
      </span>
      <span className="sr-only">{change.description}</span>
    </>
  )
}

// --- release dates ---------------------------------------------------------

/**
 * A release date at the precision Spotify actually knows it to: `2016`,
 * `May 2016` or `20 May 2016`. Formatted in UTC because a release is a calendar
 * date rather than an instant, and shifting it into a timezone would move some
 * albums a day earlier for no reason.
 */
export function formatRelease(date: string | null, precision: string): string {
  if (!date) return EMPTY
  const iso = date.slice(0, 10)
  if (precision === 'year' || iso.length === 4) return iso.slice(0, 4)
  if (precision === 'month' || iso.length === 7) return formatMonth(`${iso}-01T00:00:00Z`, 'UTC')
  return formatDate(`${iso}T00:00:00Z`, 'UTC')
}

// --- the compact ledger the detail pages use -------------------------------

export interface EntityRow {
  key: string
  to: string
  name: string
  imageUrl: string
  /** A second line under the name: artists, an album, a release year. */
  meta?: string
  plays: number
  msPlayed: number
  rank: number
}

export interface EntityLedgerProps {
  /** Describes the table for a screen reader. */
  caption: string
  /** Header over the name column: "Track", "Album". */
  column: string
  kind: ArtworkKind
  rows: EntityRow[]
  /** Set false where every row would carry the same picture. */
  artwork?: boolean
}

/**
 * The short ranked table a detail page shows: no movement column, because a
 * within-an-artist ranking has no preceding period to move against.
 */
export function EntityLedger({
  caption,
  column,
  kind,
  rows,
  artwork = true,
}: EntityLedgerProps): ReactElement {
  return (
    <Ledger caption={caption}>
      <LedgerHead>
        <LedgerRow>
          <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
          {artwork ? (
            <LedgerHeaderCell className="w-12">
              <span className="sr-only">Artwork</span>
            </LedgerHeaderCell>
          ) : null}
          <LedgerHeaderCell>{column}</LedgerHeaderCell>
          <LedgerHeaderCell numeric aria-sort="descending">
            Plays
          </LedgerHeaderCell>
          <LedgerHeaderCell numeric>Time</LedgerHeaderCell>
        </LedgerRow>
      </LedgerHead>
      <LedgerBody>
        {rows.map((row) => (
          <LedgerRow key={row.key}>
            <LedgerCell>
              <LedgerRank rank={row.rank} />
            </LedgerCell>
            {artwork ? (
              <LedgerCell>
                <Artwork src={row.imageUrl} kind={kind} size={32} />
              </LedgerCell>
            ) : null}
            <LedgerRowHeader className={ROW_HEADER}>
              <Link
                to={row.to}
                className="block max-w-[10rem] truncate text-ink hover:text-lamp sm:max-w-[22rem]"
              >
                {row.name}
              </Link>
              {row.meta ? (
                <span className="block max-w-[10rem] truncate text-xs text-ink-muted sm:max-w-[22rem]">
                  {row.meta}
                </span>
              ) : null}
            </LedgerRowHeader>
            <LedgerCell numeric>{formatCount(row.plays)}</LedgerCell>
            <LedgerCell numeric className="whitespace-nowrap">
              {formatDuration(row.msPlayed)}
            </LedgerCell>
          </LedgerRow>
        ))}
      </LedgerBody>
    </Ledger>
  )
}

// --- the figures every entity page opens with ------------------------------

export interface EntityFiguresProps {
  stats: EntityStats
  timeZone: string
  /** The entity's own name, so the chart says what it is counting. */
  subject: string
  busy?: boolean
}

/**
 * Plays, listening time, first and last listen, and the per-day timeline —
 * identical on all three entity pages, so written once.
 */
export function EntityFigures({
  stats,
  timeZone,
  subject,
  busy = false,
}: EntityFiguresProps): ReactElement {
  const [metric, setMetric] = useState<TimelineMetric>('plays')

  return (
    <>
      <StatGrid columns={4}>
        <Stat label="Plays" value={formatCount(stats.plays)} lamp />
        <Stat label="Listening time" value={formatDuration(stats.msPlayed)} />
        {/*
          These two ignore the selected range on purpose. "First listen" is a
          fact about the music, not about the window somebody happened to pick —
          reading it from the range made a track loved for a decade claim to have
          been discovered last month. The plays and listening time beside them
          are still the range's, which is the useful split.
        */}
        <Stat
          label="First listen"
          value={stats.discoveredAt ? formatDate(stats.discoveredAt, timeZone) : EMPTY}
          hint={stats.discoveredAt ? `${formatRelative(stats.discoveredAt)} · all time` : 'Never'}
        />
        <Stat
          label="Last listen"
          value={stats.lastPlayedAt ? formatDate(stats.lastPlayedAt, timeZone) : EMPTY}
          hint={stats.lastPlayedAt ? `${formatRelative(stats.lastPlayedAt)} · all time` : 'Never'}
        />
      </StatGrid>

      <ChartCard
        title="Listening over time"
        description={`Your plays of ${subject}, one point per day, in your timezone.`}
        control={<MetricToggle value={metric} onChange={setMetric} />}
      >
        <TimelineChart
          buckets={stats.timeline}
          interval="day"
          timeZone={timeZone}
          metric={metric}
          busy={busy}
          emptyAction={
            <ButtonLink to="/history" variant="primary">
              Browse your history
            </ButtonLink>
          }
        />
      </ChartCard>
    </>
  )
}

// --- the top lists ---------------------------------------------------------

export type TopKind = 'tracks' | 'artists' | 'albums'

type TopEntity = TrackRef | ArtistRef | AlbumRef

function isTrack(entity: TopEntity): entity is TrackRef {
  return 'artists' in entity
}

function isAlbum(entity: TopEntity): entity is AlbumRef {
  return 'releaseDate' in entity
}

/**
 * Where a row's picture comes from. A track carries no artwork of its own — it
 * borrows its album's — and a track whose album never resolved carries none.
 */
function imageOf(entity: TopEntity): string {
  if (isTrack(entity)) return entity.album?.imageUrl ?? ''
  return entity.imageUrl
}

/** What a row's second line says, which is the only thing the three lists differ in. */
function metaOf(entity: TopEntity): string {
  if (isTrack(entity)) return entity.artists.map((artist) => artist.name).join(', ')
  if (isAlbum(entity)) return formatRelease(entity.releaseDate, entity.releasePrecision)
  return ''
}

interface TopCopy {
  title: string
  /** Plural noun, for prose. */
  nouns: string
  /** Header over the name column. */
  column: string
  /** Header over the second column, or null where the entity has no second line. */
  metaColumn: string | null
  kind: ArtworkKind
  icon: IconName
  /** Where a row links to. */
  path: string
}

const COPY: Record<TopKind, TopCopy> = {
  tracks: {
    title: 'Top tracks',
    nouns: 'tracks',
    column: 'Track',
    metaColumn: 'Artists',
    kind: 'track',
    icon: 'track',
    path: '/tracks',
  },
  artists: {
    title: 'Top artists',
    nouns: 'artists',
    column: 'Artist',
    metaColumn: null,
    kind: 'artist',
    icon: 'artist',
    path: '/artists',
  },
  albums: {
    title: 'Top albums',
    nouns: 'albums',
    column: 'Album',
    metaColumn: 'Released',
    kind: 'album',
    icon: 'album',
    path: '/albums',
  },
}

function readOffset(value: string | null): number {
  const parsed = Number.parseInt(value ?? '0', 10)
  if (!Number.isFinite(parsed) || parsed <= 0) return 0
  // Snapped to the page grid: a hand-edited `?offset=7` would otherwise put the
  // pagination control's own arithmetic permanently out of step with the rows.
  return Math.floor(parsed / PAGE_SIZE) * PAGE_SIZE
}

export interface TopListProps {
  kind: TopKind
}

export function TopList({ kind }: TopListProps): ReactElement {
  const copy = COPY[kind]
  const { range, label } = useRange()
  const [params, setParams] = useSearchParams()
  const offset = readOffset(params.get('offset'))

  const page = useMemo(() => ({ limit: PAGE_SIZE, offset }), [offset])

  const query = useQuery({
    queryKey: qk.top(kind, range, page),
    queryFn: ({ signal }) =>
      api.get<Page<TopEntry<TopEntity>>>(
        `/stats/top/${kind}`,
        { from: range.from, to: range.to, limit: page.limit, offset: page.offset },
        signal,
      ),
  })

  const setOffset = (next: number): void => {
    setParams((current) => {
      const updated = new URLSearchParams(current)
      if (next <= 0) updated.delete('offset')
      else updated.set('offset', String(next))
      return updated
    })
  }

  const items = query.data?.items ?? []
  const total = query.data?.total ?? 0
  const noun = copy.nouns.slice(0, -1)
  const first = total === 0 ? 0 : offset + 1
  const last = Math.min(offset + PAGE_SIZE, total)

  // A bookmarked or hand-edited offset can land past the last row. That is not
  // an empty range, and saying so would send someone widening dates for nothing.
  const pastEnd = query.isSuccess && items.length === 0 && total > 0

  const status = query.isPending
    ? `Loading your top ${copy.nouns}.`
    : query.isError
      ? `Your top ${copy.nouns} could not be loaded.`
      : pastEnd
        ? `That page is past the end. This range holds ${formatPlural(total, noun)}.`
        : total === 0
          ? `No ${copy.nouns} played in ${label.toLowerCase()}.`
          : `Showing ${formatCount(first)} to ${formatCount(last)} of ${formatPlural(total, noun)}, ranked by plays.`

  return (
    <div className="space-y-4">
      <PageHeader
        title={copy.title}
        description={`Your most played ${copy.nouns}, ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <Panel
        title="Ranked by plays"
        description="Highest first. Movement compares with the equal-length period before this range."
        padded={false}
      >
        {query.isPending ? (
          <SkeletonLedger rows={12} columns={5} />
        ) : query.isError ? (
          <ErrorState
            error={query.error}
            title={`Your top ${copy.nouns} could not be loaded`}
            onRetry={() => {
              void query.refetch()
            }}
          />
        ) : pastEnd ? (
          <EmptyState
            icon={copy.icon}
            title="That page is past the end"
            description={`This range holds ${formatPlural(total, noun)}, and this page starts after the last one.`}
            action={
              <Button variant="primary" onClick={() => setOffset(0)}>
                Back to the first page
              </Button>
            }
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon={copy.icon}
            title={`No ${copy.nouns} in this range`}
            description="Nothing was played between these dates. Widen the range above, or import more of your history."
            action={
              <ButtonLink to="/imports" variant="primary">
                Import your history
              </ButtonLink>
            }
          />
        ) : (
          <div
            className={query.isFetching ? 'opacity-60 transition-opacity' : 'transition-opacity'}
          >
            <Ledger caption={`Your most played ${copy.nouns} in this range, ranked by plays`}>
              <LedgerHead>
                <LedgerRow>
                  <LedgerHeaderCell className="w-10">Rank</LedgerHeaderCell>
                  <LedgerHeaderCell className="w-12">
                    <span className="sr-only">Artwork</span>
                  </LedgerHeaderCell>
                  <LedgerHeaderCell>{copy.column}</LedgerHeaderCell>
                  {copy.metaColumn ? (
                    <LedgerHeaderCell className="hidden md:table-cell">
                      {copy.metaColumn}
                    </LedgerHeaderCell>
                  ) : null}
                  <LedgerHeaderCell numeric aria-sort="descending">
                    Plays
                  </LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Time</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Move</LedgerHeaderCell>
                </LedgerRow>
              </LedgerHead>
              <LedgerBody>
                {items.map((entry) => {
                  const meta = metaOf(entry.entity)
                  return (
                    <LedgerRow key={entry.entity.id}>
                      <LedgerCell>
                        <LedgerRank rank={entry.rank} />
                      </LedgerCell>
                      <LedgerCell>
                        <Artwork src={imageOf(entry.entity)} kind={copy.kind} size={36} />
                      </LedgerCell>
                      {/* `.ledger th` sets every header cell in the eyebrow face,
                          which is right for a column heading and wrong for the
                          name of a row, so the row header opts back out. */}
                      <LedgerRowHeader className={ROW_HEADER}>
                        <Link
                          to={`${copy.path}/${entry.entity.id}`}
                          className="block max-w-[10rem] truncate text-ink hover:text-lamp sm:max-w-[18rem]"
                        >
                          {entry.entity.name}
                        </Link>
                        {meta ? (
                          // The same text as the column beside it, and never both
                          // at once: one of the two is display:none at any width,
                          // so it is read once.
                          <span className="block max-w-[10rem] truncate text-xs text-ink-muted md:hidden">
                            {meta}
                          </span>
                        ) : null}
                      </LedgerRowHeader>
                      {copy.metaColumn ? (
                        <LedgerCell className="hidden md:table-cell">
                          <span className="block max-w-[16rem] truncate text-ink-muted">
                            {meta || EMPTY}
                          </span>
                        </LedgerCell>
                      ) : null}
                      <LedgerCell numeric>{formatCount(entry.plays)}</LedgerCell>
                      <LedgerCell numeric className="whitespace-nowrap">
                        {formatDuration(entry.msPlayed)}
                      </LedgerCell>
                      <LedgerCell numeric>
                        <Movement rank={entry.rank} previousRank={entry.previousRank} />
                      </LedgerCell>
                    </LedgerRow>
                  )
                })}
              </LedgerBody>
            </Ledger>

            <Pagination
              total={total}
              limit={PAGE_SIZE}
              offset={offset}
              onChange={setOffset}
              label={copy.title}
              disabled={query.isFetching}
              className="border-t border-seam"
            />
          </div>
        )}
      </Panel>
    </div>
  )
}
