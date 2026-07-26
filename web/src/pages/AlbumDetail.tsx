/**
 * OWNED BY THE PAGES AGENT — placeholder.
 *
 * The shell routes to this module so the application builds and navigates as a
 * whole. Replace the whole file with the real page; the router needs no change.
 */

import type { ReactElement } from 'react'
import { Placeholder } from './Placeholder'

export default function AlbumDetail(): ReactElement {
  return (
    <Placeholder
      title="Album"
      summary="One album: its tracks, its artists, and how often it has been played."
      icon="album"
    />
  )
}
