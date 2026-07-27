/**
 * Carrying the selected range across a navigation.
 *
 * The range lives in the query string so that any view is linkable, which is
 * also why an ordinary link loses it: choose 2019, click an artist, and the
 * artist page finds no `from` and quietly shows the default thirty days. The
 * figures then describe a period nobody asked for and nothing on screen says so.
 */

import type { ReactElement } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { RangeLink } from '../components/ui/RangeLink'

function at(search: string, children: ReactElement): ReactElement {
  return <MemoryRouter initialEntries={[`/artists${search}`]}>{children}</MemoryRouter>
}

const FROM = '2019-01-01T00:00:00.000Z'
const TO = '2020-01-01T00:00:00.000Z'

describe('a range-carrying link', () => {
  it('takes the active range to the destination', () => {
    render(
      at(
        `?from=${encodeURIComponent(FROM)}&to=${encodeURIComponent(TO)}`,
        <RangeLink to="/artists/abc">Massive Attack</RangeLink>,
      ),
    )

    const href = screen.getByRole('link').getAttribute('href') ?? ''
    const url = new URL(href, 'http://encore.test')
    expect(url.pathname).toBe('/artists/abc')
    expect(url.searchParams.get('from')).toBe(FROM)
    expect(url.searchParams.get('to')).toBe(TO)
  })

  it('leaves a path alone when no range is selected', () => {
    render(at('', <RangeLink to="/artists/abc">Massive Attack</RangeLink>))
    expect(screen.getByRole('link')).toHaveAttribute('href', '/artists/abc')
  })

  it('does not overwrite a range the link already carries', () => {
    // An explicit range in a link is a deliberate one — the year-in-review
    // pages build such links on purpose.
    render(
      at(
        `?from=${encodeURIComponent(FROM)}&to=${encodeURIComponent(TO)}`,
        <RangeLink to="/artists/abc?from=2024-01-01T00%3A00%3A00.000Z&to=2025-01-01T00%3A00%3A00.000Z">
          Elsewhere
        </RangeLink>,
      ),
    )

    const url = new URL(screen.getByRole('link').getAttribute('href') ?? '', 'http://encore.test')
    expect(url.searchParams.get('from')).toBe('2024-01-01T00:00:00.000Z')
  })

  it('keeps any other parameters the destination asked for', () => {
    render(
      at(
        `?from=${encodeURIComponent(FROM)}&to=${encodeURIComponent(TO)}`,
        <RangeLink to="/artists/abc?tab=albums">With a tab</RangeLink>,
      ),
    )

    const url = new URL(screen.getByRole('link').getAttribute('href') ?? '', 'http://encore.test')
    expect(url.searchParams.get('tab')).toBe('albums')
    expect(url.searchParams.get('from')).toBe(FROM)
  })

  it('does not carry a half-set range', () => {
    // `from` without `to` is not a range, and passing it on would produce a
    // request the API refuses rather than a view somebody asked for.
    render(at(`?from=${encodeURIComponent(FROM)}`, <RangeLink to="/artists/abc">Half</RangeLink>))
    expect(screen.getByRole('link')).toHaveAttribute('href', '/artists/abc')
  })
})
