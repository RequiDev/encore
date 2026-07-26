/**
 * OWNED BY THE PAGES AGENT — placeholder.
 *
 * The shell routes to this module so the application builds and navigates as a
 * whole. Replace the whole file with the real page; the router needs no change.
 */

import type { ReactElement } from 'react'
import { Placeholder } from './Placeholder'

export default function History(): ReactElement {
  return (
    <Placeholder
      title="Listening history"
      summary="Every listen, newest first, paged by cursor."
      icon="history"
    />
  )
}
