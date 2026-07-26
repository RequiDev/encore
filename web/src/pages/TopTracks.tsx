/**
 * The most played tracks in the range.
 *
 * The table, the pagination and the four states are `TopList`, which the artist
 * and album lists render too; this file is the configuration for one of them.
 */

import type { ReactElement } from 'react'
import { TopList } from './top/TopList'

export default function TopTracks(): ReactElement {
  return <TopList kind="tracks" />
}
