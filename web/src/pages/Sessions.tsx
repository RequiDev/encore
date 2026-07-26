/**
 * OWNED BY THE PAGES AGENT — placeholder.
 *
 * The shell routes to this module so the application builds and navigates as a
 * whole. Replace the whole file with the real page; the router needs no change.
 */

import type { ReactElement } from 'react'
import { Placeholder } from './Placeholder'

export default function Sessions(): ReactElement {
  return (
    <Placeholder
      title="Sessions"
      summary="The longest unbroken runs of listening, and what was played in them."
      icon="session"
    />
  )
}
