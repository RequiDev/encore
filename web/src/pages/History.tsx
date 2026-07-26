/**
 * The listening feed: every play, newest first.
 *
 * This is the one page in Encore that is keyset paginated rather than offset
 * paginated, because a person with a full extended export legitimately holds
 * millions of rows and `OFFSET 900000` makes the database walk and discard every
 * skipped one. The cursor the server hands back is opaque: it is held in the
 * query cache and passed back exactly as it arrived, never parsed and never
 * constructed here.
 *
 * `useInfiniteQuery` is what keeps the loaded set stable. Each answered page is
 * appended to the ones already on screen rather than replacing them, so pressing
 * "Load older" never moves a row a person was reading — and changing the date
 * range starts a new cache entry rather than mixing two ranges in one list.
 */

import type { ReactElement } from 'react'
import { useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useInfiniteQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useRange } from '../lib/range'
import { formatCount, formatDateTime, formatDuration, formatPlural } from '../lib/format'
import type { HistoryItem, HistoryResponse, ListenSource } from '../lib/types'
import {
  Button,
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Panel,
  RangePicker,
  SkeletonLedger,
} from '../components/ui'
import type { ChipTone } from '../components/ui'

/** The server's own default page. Fifty rows is about two screens of table. */
const PAGE_SIZE = 50

/** How each listen reached Encore, in the reader's words rather than the API's. */
const SOURCE_LABEL: Record<ListenSource, string> = {
  sync: 'Sync',
  account_data: 'Account data',
  extended: 'Extended',
}

/**
 * Tone never carries meaning on its own here — the chip always says the word —
 * but a live sync is worth distinguishing from the two import formats at a
 * glance, since it is the only source that keeps arriving.
 */
const SOURCE_TONE: Record<ListenSource, ChipTone> = {
  sync: 'info',
  account_data: 'neutral',
  extended: 'neutral',
}

export default function History(): ReactElement {
  const { range, label, timeZone, setPreset } = useRange()

  const history = useInfiniteQuery({
    queryKey: qk.history(range, PAGE_SIZE),
    initialPageParam: null as string | null,
    queryFn: ({ pageParam, signal }) =>
      api.get<HistoryResponse>(
        '/history',
        { from: range.from, to: range.to, limit: PAGE_SIZE, cursor: pageParam },
        signal,
      ),
    // Opaque in, opaque out. `hasMore` is the server's answer, never inferred
    // from a short page — a page can be short and still have more behind it.
    getNextPageParam: (last: HistoryResponse) => (last.hasMore ? last.nextCursor : null),
  })

  const items = useMemo(
    () => (history.data?.pages ?? []).flatMap((page) => page.items),
    [history.data],
  )

  const status = history.isPending
    ? `Loading your listening history for ${label.toLowerCase()}.`
    : history.isError
      ? 'Your listening history could not be loaded.'
      : `${formatPlural(items.length, 'listen')} loaded${history.hasNextPage ? ', more available' : ''}.`

  return (
    <div className="space-y-4">
      <PageHeader
        title="Listening history"
        description={`Every listen, newest first — ${label.toLowerCase()}.`}
        actions={<RangePicker />}
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <Panel
        title="Listens"
        description="Source says how a listen reached Encore: a live sync from Spotify, your account-data export, or your extended streaming history."
        padded={false}
      >
        {history.isPending ? (
          <SkeletonLedger rows={12} columns={5} />
        ) : history.isError ? (
          <ErrorState
            error={history.error}
            title="Your listening history could not be loaded"
            onRetry={() => {
              void history.refetch()
            }}
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon="history"
            title="No listens in this range"
            description="Nothing was played between these dates. Widen the range, or import more of your history."
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
          <>
            <Ledger caption={`Every listen in ${label.toLowerCase()}, newest first`}>
              <LedgerHead>
                <LedgerRow>
                  <LedgerHeaderCell>Played</LedgerHeaderCell>
                  <LedgerHeaderCell>Track</LedgerHeaderCell>
                  <LedgerHeaderCell>Artists</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Listened</LedgerHeaderCell>
                  <LedgerHeaderCell>Source</LedgerHeaderCell>
                </LedgerRow>
              </LedgerHead>
              <LedgerBody>
                {items.map((item) => (
                  <HistoryRow key={item.id} item={item} timeZone={timeZone} />
                ))}
              </LedgerBody>
            </Ledger>

            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-seam px-4 py-3">
              <p className="text-xs text-ink-muted">
                <span className="tabular">{formatCount(items.length)}</span> loaded
                {history.hasNextPage ? '' : ' — that is everything in this range'}
              </p>
              {history.hasNextPage ? (
                <Button
                  busy={history.isFetchingNextPage}
                  onClick={() => {
                    void history.fetchNextPage()
                  }}
                >
                  Load older listens
                </Button>
              ) : null}
            </div>
          </>
        )}
      </Panel>

      {/* A page that failed only on its second page keeps the rows it has and
          says what went wrong under them, rather than throwing them away. */}
      {items.length > 0 && history.isFetchNextPageError ? (
        <Panel padded={false}>
          <ErrorState
            error={history.error}
            title="The next page could not be loaded"
            onRetry={() => {
              void history.fetchNextPage()
            }}
          />
        </Panel>
      ) : null}
    </div>
  )
}

/**
 * One listen.
 *
 * A listen whose track is null is a names-only record: the import carried the
 * artist and title as text and the catalogue lookup has not resolved them yet.
 * Showing those names is honest; linking them would not be, because there is
 * nothing to link to.
 */
function HistoryRow({ item, timeZone }: { item: HistoryItem; timeZone: string }): ReactElement {
  const track = item.track
  const artists = track ? track.artists : null

  return (
    <LedgerRow>
      <LedgerCell className="whitespace-nowrap">
        <time dateTime={item.playedAt} className="tabular text-ink-muted">
          {formatDateTime(item.playedAt, timeZone)}
        </time>
      </LedgerCell>

      <LedgerRowHeader>
        {track ? (
          <Link
            to={`/tracks/${track.id}`}
            className="block max-w-[14rem] truncate text-ink hover:text-lamp sm:max-w-[24rem]"
          >
            {track.name}
          </Link>
        ) : (
          <>
            <span className="block max-w-[14rem] truncate text-ink sm:max-w-[24rem]">
              {item.aliasTitle ?? 'Untitled'}
            </span>
            <span className="block text-xs text-ink-faint">
              Details are still being fetched from Spotify.
            </span>
          </>
        )}
      </LedgerRowHeader>

      <LedgerCell>
        {artists && artists.length > 0 ? (
          <span className="block max-w-[12rem] truncate sm:max-w-[18rem]">
            {artists.map((artist, index) => (
              <span key={artist.id}>
                {index > 0 ? <span className="text-ink-faint">, </span> : null}
                <Link to={`/artists/${artist.id}`} className="text-ink-muted hover:text-lamp">
                  {artist.name}
                </Link>
              </span>
            ))}
          </span>
        ) : (
          <span className="block max-w-[12rem] truncate text-ink-muted sm:max-w-[18rem]">
            {item.aliasArtist ?? 'Unknown artist'}
          </span>
        )}
      </LedgerCell>

      <LedgerCell numeric className="whitespace-nowrap">
        {formatDuration(item.msPlayed)}
      </LedgerCell>

      <LedgerCell>
        <Chip tone={SOURCE_TONE[item.source]}>{SOURCE_LABEL[item.source]}</Chip>
      </LedgerCell>
    </LedgerRow>
  )
}
