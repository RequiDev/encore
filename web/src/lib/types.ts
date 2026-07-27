/**
 * TypeScript mirror of Encore's HTTP API payloads.
 *
 * This file is the client half of the contract documented in `docs/api.md`; the
 * server half lives in `internal/httpapi/dto.go`. They are kept in step by hand,
 * so change all three together.
 */

// --- primitives ------------------------------------------------------------

/** RFC 3339 timestamp with a `Z` offset. */
export type Timestamp = string

export type Role = 'user' | 'admin'
export type SyncState = 'ok' | 'needs_reauth' | 'error'
export type ListenSource = 'sync' | 'account_data' | 'extended'
export type Interval = 'hour' | 'day' | 'week' | 'month' | 'year'

export type ImportStatus = 'queued' | 'running' | 'paused' | 'completed' | 'failed' | 'cancelled'
export type ImportFileStatus = 'pending' | 'running' | 'completed' | 'failed' | 'skipped'
export type ImportFormat = 'extended' | 'account_data' | 'unknown'

export interface ApiErrorBody {
  error: { code: string; message: string; details?: Record<string, string> }
}

// --- identity --------------------------------------------------------------

export interface User {
  id: string
  spotifyUserId: string
  displayName: string
  email: string
  avatarUrl: string
  role: Role
  isActive: boolean
  timezone: string
  createdAt: Timestamp
  lastLoginAt: Timestamp | null
}

export interface SpotifyConnection {
  connected: boolean
  syncState: SyncState
  lastSyncAt: Timestamp | null
  lastSyncError: string
  scopes: string[]
}

export interface InstanceInfo {
  registrationsEnabled: boolean
  version: string
}

/**
 * The span of history a user holds. Both null before they import or sync
 * anything. The client uses `firstListenAt` so that an "all time" range starts
 * where the history does, rather than at a fixed floor that would draw years of
 * empty buckets.
 */
export interface ListeningBounds {
  firstListenAt: Timestamp | null
  lastListenAt: Timestamp | null
}

/** Response of `GET /api/me`, the client's bootstrap call. */
export interface MeResponse {
  user: User
  spotify: SpotifyConnection
  csrfToken: string
  instance: InstanceInfo
  listening: ListeningBounds
}

/** A user other than the caller, for the comparison page. Deliberately minimal. */
export interface PublicUser {
  id: string
  displayName: string
  avatarUrl: string
}

export interface AdminUser extends User {
  listenCount: number
  syncState: SyncState
  lastSyncAt: Timestamp | null
}

// --- catalogue -------------------------------------------------------------

export interface ArtistRef {
  id: string
  name: string
  imageUrl: string
}

export interface AlbumRef {
  id: string
  name: string
  imageUrl: string
  releaseDate: string | null
  releasePrecision: string
}

export interface TrackRef {
  id: string
  name: string
  durationMs: number
  explicit: boolean
  album: AlbumRef | null
  artists: ArtistRef[]
}

export interface Artist extends ArtistRef {
  genres: string[]
  popularity: number
  followers: number
}

export interface Album extends AlbumRef {
  albumType: string
  totalTracks: number
  artists: ArtistRef[]
}

export interface SearchResponse {
  artists: ArtistRef[]
  albums: AlbumRef[]
  tracks: TrackRef[]
}

// --- statistics ------------------------------------------------------------

export interface Summary {
  listens: number
  distinctTracks: number
  distinctArtists: number
  distinctAlbums: number
  msPlayed: number
  activeDays: number
  firstListenAt: Timestamp | null
  lastListenAt: Timestamp | null
}

/**
 * One row of a top-N list. `previousRank` is null when the entity did not appear
 * in the equal-length preceding period, which the UI renders as "new" rather
 * than as a rise from infinity.
 */
export interface TopEntry<T> {
  entity: T
  plays: number
  msPlayed: number
  rank: number
  previousRank: number | null
}

export interface Page<T> {
  items: T[]
  total: number
}

export type TopTracks = Page<TopEntry<TrackRef>>
export type TopArtists = Page<TopEntry<ArtistRef>>
export type TopAlbums = Page<TopEntry<AlbumRef>>

export interface TimelineBucket {
  bucket: Timestamp
  plays: number
  msPlayed: number
  distinctTracks: number
  distinctArtists: number
}

export interface TimelineResponse {
  interval: Interval
  buckets: TimelineBucket[]
}

export interface RepartitionBucket {
  /** Hour 0-23, or weekday 0-6 with 0 = Monday. */
  key: number
  plays: number
  msPlayed: number
}

export interface HeatmapCell {
  weekday: number
  hour: number
  plays: number
  msPlayed: number
}

export interface ListeningSession {
  startedAt: Timestamp
  endedAt: Timestamp
  trackCount: number
  msPlayed: number
  tracks: TrackRef[]
}

export interface DiscoveryBucket {
  bucket: Timestamp
  newArtists: number
  newTracks: number
}

export interface Streak {
  startDay: string
  endDay: string
  days: number
}

export interface StreaksResponse {
  current: Streak | null
  longest: Streak | null
  top: Streak[]
}

export interface CompareResponse {
  a: { from: Timestamp; to: Timestamp; summary: Summary }
  b: { from: Timestamp; to: Timestamp; summary: Summary }
  delta: {
    listens: number
    msPlayed: number
    distinctTracks: number
    distinctArtists: number
    distinctAlbums: number
  }
}

export interface YearInReview {
  year: number
  summary: Summary
  topTracks: TopEntry<TrackRef>[]
  topArtists: TopEntry<ArtistRef>[]
  topAlbums: TopEntry<AlbumRef>[]
  busiestDay: { day: string; plays: number; msPlayed: number } | null
  longestSession: ListeningSession | null
  newArtists: number
}

export interface StatsExtras {
  differentArtists: number
  averageAlbumReleaseYear: number | null
  averageArtistsPerTrack: number
}

export interface AffinityEntry<T> {
  entity: T
  playsA: number
  playsB: number
}

export interface AffinityResponse {
  user: PublicUser
  score: number
  artists: AffinityEntry<ArtistRef>[]
  albums: AffinityEntry<AlbumRef>[]
  tracks: AffinityEntry<TrackRef>[]
}

export interface EntityStats {
  plays: number
  msPlayed: number
  firstListenAt: Timestamp | null
  lastListenAt: Timestamp | null
  timeline: TimelineBucket[]
}

export interface TrackDetail {
  track: TrackRef
  stats: EntityStats
}

export interface ArtistDetail {
  artist: Artist
  stats: EntityStats
  /** The artist's share of the caller's total listening time in the range, 0-1. */
  share: number
  topTracks: TopEntry<TrackRef>[]
  topAlbums: TopEntry<AlbumRef>[]
  hourRepartition: RepartitionBucket[]
  blacklisted: boolean
}

export interface AlbumDetail {
  album: Album
  stats: EntityStats
  topTracks: TopEntry<TrackRef>[]
}

// --- listening history -----------------------------------------------------

export interface HistoryItem {
  id: string
  playedAt: Timestamp
  msPlayed: number
  source: ListenSource
  /** Null while the listen is still a names-only record awaiting alias resolution. */
  track: TrackRef | null
  aliasArtist: string | null
  aliasTitle: string | null
}

export interface HistoryResponse {
  items: HistoryItem[]
  /** Opaque. Pass it back verbatim; never construct one. */
  nextCursor: string | null
  hasMore: boolean
}

// --- imports ---------------------------------------------------------------

export interface Counters {
  imported: number
  duplicates: number
  skipped: number
  rejected: number
}

export interface ImportFile {
  id: string
  name: string
  containerPath: string
  format: ImportFormat
  status: ImportFileStatus
  sizeBytes: number
  /** Null until the file has been read to the end. */
  recordsTotal: number | null
  recordOffset: number
  /** Null while recordsTotal is unknown; the UI shows "counting", not a fake denominator. */
  pending: number | null
  counters: Counters
  errorCode: string
  errorMessage: string
  startedAt: Timestamp | null
  finishedAt: Timestamp | null
}

export interface ImportJob {
  id: string
  status: ImportStatus
  note: string
  createdAt: Timestamp
  startedAt: Timestamp | null
  finishedAt: Timestamp | null
  errorCode: string
  errorMessage: string
  filesTotal: number
  filesDone: number
  counters: Counters
  files: ImportFile[]
}

export interface ImportWarning {
  file: string
  code: string
  message: string
}

export interface CreateImportResponse {
  job: ImportJob
  warnings: ImportWarning[]
}

export interface ImportReject {
  recordIndex: number
  reason: string
  detail: string
  rawExcerpt: string
  createdAt: Timestamp
}

// --- misc ------------------------------------------------------------------

/**
 * What `POST /api/sync/now` reports back. Spotify's recently-played feed only
 * reaches back fifty plays, so a manual sync usually returns mostly duplicates;
 * the counts exist so "nothing new" reads as a normal result.
 */
export interface SyncOutcome {
  fetched: number
  imported: number
  duplicates: number
  skipped: number
  newestAt: Timestamp | null
}

export interface AdminSettings {
  registrationsEnabled: boolean
}

export interface HealthResponse {
  status: string
  checks?: Record<string, string>
}
