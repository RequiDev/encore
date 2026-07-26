/**
 * OWNED BY THE PAGES AGENT — placeholder.
 *
 * The shell routes to this module so the application builds and navigates as a
 * whole. Replace the whole file with the real page; the router needs no change.
 */

import type { ReactElement } from 'react'
import { Placeholder } from './Placeholder'

export default function Streaks(): ReactElement {
  return (
    <Placeholder
      title="Streaks"
      summary="Runs of consecutive days with at least one listen."
      icon="streak"
    />
  )
}
