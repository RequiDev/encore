/**
 * OWNED BY THE PAGES AGENT — placeholder.
 *
 * The shell routes to this module so the application builds and navigates as a
 * whole. Replace the whole file with the real page; the router needs no change.
 */

import type { ReactElement } from 'react'
import { Placeholder } from './Placeholder'

export default function TopAlbums(): ReactElement {
  return (
    <Placeholder
      title="Top albums"
      summary="The most played albums in the range, with movement against the preceding period."
      icon="album"
    />
  )
}
