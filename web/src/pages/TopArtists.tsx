/**
 * The most played artists in the range.
 *
 * The table, the pagination and the four states are `TopList`, which the track
 * and album lists render too; this file is the configuration for one of them.
 */

import type { ReactElement } from 'react'
import { TopList } from './top/TopList'

export default function TopArtists(): ReactElement {
  return <TopList kind="artists" />
}
