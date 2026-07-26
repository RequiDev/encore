/**
 * Search.
 *
 * One field, debounced, hitting `/api/search`. The query lives in the URL, so a
 * search can be bookmarked or sent to someone; typing rewrites it in place
 * rather than filling the back button with a history entry per keystroke.
 *
 * Results are three groups of links. The arrow keys walk them without leaving
 * the keyboard, and the count is announced, because a list that appears silently
 * under a text field is invisible to anyone not looking at it.
 */

import type { KeyboardEvent, ReactElement, ReactNode } from 'react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useDebouncedValue } from '../lib/hooks'
import { formatCount, formatPlural } from '../lib/format'
import type { SearchResponse } from '../lib/types'
import {
  Button,
  ButtonLink,
  EmptyState,
  ErrorState,
  Field,
  Icon,
  Input,
  PageHeader,
  Panel,
  SkeletonText,
} from '../components/ui'
import { Artwork, formatRelease } from './top/TopList'
import type { ArtworkKind } from './top/TopList'

/** Below this a query matches most of the catalogue and helps nobody. */
const MIN_QUERY = 2

/** Per group, not in total: ten artists and ten tracks fit on one screen. */
const LIMIT = 10

interface Result {
  key: string
  to: string
  name: string
  meta: string
  imageUrl: string
}

export default function Search(): ReactElement {
  const [params, setParams] = useSearchParams()
  const [text, setText] = useState(() => params.get('q') ?? '')
  const term = useDebouncedValue(text.trim(), 250)
  const field = useRef<HTMLDivElement>(null)

  // The URL follows the debounced value, not every keystroke, and only when it
  // has actually changed — writing the same value back would be a navigation
  // loop rather than a no-op.
  useEffect(() => {
    if ((params.get('q') ?? '') === term) return
    setParams(
      (current) => {
        const updated = new URLSearchParams(current)
        if (term) updated.set('q', term)
        else updated.delete('q')
        return updated
      },
      { replace: true },
    )
  }, [term, params, setParams])

  const enabled = term.length >= MIN_QUERY

  const query = useQuery({
    queryKey: qk.search(term, LIMIT),
    queryFn: ({ signal }) => api.get<SearchResponse>('/search', { q: term, limit: LIMIT }, signal),
    enabled,
  })

  const data = query.data
  const artists = useMemo<Result[]>(
    () =>
      (data?.artists ?? []).map((artist) => ({
        key: artist.id,
        to: `/artists/${artist.id}`,
        name: artist.name,
        meta: '',
        imageUrl: artist.imageUrl,
      })),
    [data],
  )
  const albums = useMemo<Result[]>(
    () =>
      (data?.albums ?? []).map((album) => ({
        key: album.id,
        to: `/albums/${album.id}`,
        name: album.name,
        meta: formatRelease(album.releaseDate, album.releasePrecision),
        imageUrl: album.imageUrl,
      })),
    [data],
  )
  const tracks = useMemo<Result[]>(
    () =>
      (data?.tracks ?? []).map((track) => ({
        key: track.id,
        to: `/tracks/${track.id}`,
        name: track.name,
        meta: track.artists.map((artist) => artist.name).join(', '),
        imageUrl: track.album?.imageUrl ?? '',
      })),
    [data],
  )

  const total = artists.length + albums.length + tracks.length

  const announcement = !enabled
    ? `Type at least ${MIN_QUERY} characters to search.`
    : query.isPending
      ? `Searching for ${term}.`
      : query.isError
        ? 'The search could not be run.'
        : total === 0
          ? `Nothing matched ${term}.`
          : `${formatPlural(total, 'result')} for ${term}: ${formatCount(artists.length)} artists, ${formatCount(albums.length)} albums, ${formatCount(tracks.length)} tracks.`

  /**
   * Moves focus between result links. The handler sits on the links themselves
   * rather than on a wrapper, so the keyboard behaviour belongs to something
   * that was already focusable.
   */
  const step = (from: EventTarget & HTMLElement, direction: number): void => {
    const links = Array.from(document.querySelectorAll<HTMLAnchorElement>('a[data-result]'))
    const index = links.indexOf(from as HTMLAnchorElement)
    if (index < 0) return
    const next = links[Math.min(Math.max(index + direction, 0), links.length - 1)]
    if (next) next.focus()
    else field.current?.querySelector('input')?.focus()
  }

  const onResultKeyDown = (event: KeyboardEvent<HTMLAnchorElement>): void => {
    if (event.key === 'ArrowDown') {
      event.preventDefault()
      step(event.currentTarget, 1)
    } else if (event.key === 'ArrowUp') {
      event.preventDefault()
      const links = Array.from(document.querySelectorAll<HTMLAnchorElement>('a[data-result]'))
      if (links[0] === event.currentTarget) field.current?.querySelector('input')?.focus()
      else step(event.currentTarget, -1)
    }
  }

  const onFieldKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key === 'ArrowDown') {
      event.preventDefault()
      document.querySelector<HTMLAnchorElement>('a[data-result]')?.focus()
    } else if (event.key === 'Escape' && text !== '') {
      event.preventDefault()
      setText('')
    }
  }

  return (
    <div className="space-y-4">
      <PageHeader
        title="Search"
        description="Find an artist, an album or a track you have listened to."
      />

      <Panel>
        <form
          role="search"
          onSubmit={(event) => {
            // There is nothing to submit: results arrive as the field settles.
            event.preventDefault()
          }}
          className="flex flex-col gap-3 sm:flex-row sm:items-end"
        >
          <div ref={field} className="min-w-0 flex-1">
            <Field
              label="Search your catalogue"
              hint={`At least ${MIN_QUERY} characters. Press the down arrow to step into the results.`}
            >
              <Input
                type="search"
                value={text}
                onChange={(event) => setText(event.target.value)}
                onKeyDown={onFieldKeyDown}
                placeholder="Artist, album or track"
                autoComplete="off"
                spellCheck={false}
                enterKeyHint="search"
              />
            </Field>
          </div>
          <Button
            onClick={() => {
              setText('')
              field.current?.querySelector('input')?.focus()
            }}
            disabled={text === ''}
            className="shrink-0"
          >
            <Icon name="close" />
            Clear
          </Button>
        </form>
      </Panel>

      <p role="status" aria-live="polite" className="sr-only">
        {announcement}
      </p>

      {!enabled ? (
        <Panel padded={false}>
          <EmptyState
            icon="search"
            title="Type to search"
            description={`Two characters is enough to start. Encore searches the artists, albums and tracks it knows from your listening history.`}
            action={
              <ButtonLink to="/tracks" variant="primary">
                Browse your top tracks
              </ButtonLink>
            }
          />
        </Panel>
      ) : query.isPending ? (
        <div className="grid gap-4 lg:grid-cols-3" aria-busy="true">
          {['Artists', 'Albums', 'Tracks'].map((group) => (
            <Panel key={group} title={group}>
              <SkeletonText lines={5} />
            </Panel>
          ))}
        </div>
      ) : query.isError ? (
        <Panel padded={false}>
          <ErrorState
            error={query.error}
            title="The search could not be run"
            onRetry={() => {
              void query.refetch()
            }}
          />
        </Panel>
      ) : total === 0 ? (
        <Panel padded={false}>
          <EmptyState
            icon="search"
            title={`Nothing matched “${term}”`}
            description="Check the spelling, or try part of a name. Only what you have listened to is searchable."
            action={
              <ButtonLink to="/history" variant="primary">
                Browse your history
              </ButtonLink>
            }
          />
        </Panel>
      ) : (
        <div className="grid items-start gap-4 lg:grid-cols-3">
          <Group
            title="Artists"
            kind="artist"
            results={artists}
            term={term}
            onKeyDown={onResultKeyDown}
          />
          <Group
            title="Albums"
            kind="album"
            results={albums}
            term={term}
            onKeyDown={onResultKeyDown}
          />
          <Group
            title="Tracks"
            kind="track"
            results={tracks}
            term={term}
            onKeyDown={onResultKeyDown}
          />
        </div>
      )}
    </div>
  )
}

interface GroupProps {
  title: string
  kind: ArtworkKind
  results: Result[]
  term: string
  onKeyDown: (event: KeyboardEvent<HTMLAnchorElement>) => void
}

/** One group of results. An empty group still appears, so the layout holds still. */
function Group({ title, kind, results, term, onKeyDown }: GroupProps): ReactElement {
  return (
    <Panel
      title={title}
      description={countLabel(results.length, title.toLowerCase().slice(0, -1))}
      padded={false}
    >
      {results.length === 0 ? (
        <p className="px-4 py-6 text-sm text-ink-muted">{`No ${title.toLowerCase()} matched “${term}”.`}</p>
      ) : (
        <ul className="divide-y divide-seam">
          {results.map((result) => (
            <li key={result.key}>
              <Link
                to={result.to}
                data-result=""
                onKeyDown={onKeyDown}
                className="flex items-center gap-3 px-4 py-2.5 hover:bg-panel-raised"
              >
                <Artwork src={result.imageUrl} kind={kind} size={40} />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm text-ink">{result.name}</span>
                  {result.meta ? (
                    <span className="block truncate text-xs text-ink-muted">{result.meta}</span>
                  ) : null}
                </span>
                <span aria-hidden="true" className="shrink-0 text-ink-faint">
                  <Icon name="chevron-right" />
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

function countLabel(count: number, noun: string): ReactNode {
  return count === 0 ? 'None found' : formatPlural(count, noun)
}
