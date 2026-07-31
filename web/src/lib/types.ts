/**
 * TypeScript mirror of Encore's HTTP API payloads.
 *
 * This file is the client half of the contract documented in `docs/api.md`; the
 * server half lives in `internal/httpapi/dto.go`. They are kept in step by hand,
 * so change all three together.
 */

import type { LazyFetchState } from './fetchpoll'

/**
 * Re-exported so a page can name a lazy panel's state without also importing
 * the polling module for a type. `AlbumTrackListState` below is an alias of it
 * kept for the album payload's own readability; the discography uses this name
 * directly, because there is nothing album-shaped about it.
 */
export type { LazyFetchState }

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
  /**
   * Scopes this account granted less than Encore now asks for.
   *
   * Computed server-side against the required list; the client deliberately
   * holds no copy of that list, because two copies drift.
   */
  missingScopes: string[]
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

// --- sharing ---------------------------------------------------------------

/** The body of POST /api/shares. Either a fixed range or a rolling window. */
export interface CreateShareRequest {
  label?: string
  from?: Timestamp
  to?: Timestamp
  days?: number
  expiresAt?: Timestamp
}

/**
 * One link, as its owner sees it.
 *
 * `token` and `url` arrive only in the response that creates the link: the
 * server stores nothing but a hash, so a listing cannot reconstruct them and
 * does not pretend to.
 */
export interface ShareLink {
  id: string
  label: string
  token?: string
  url?: string
  rolling: boolean
  rangeDays: number
  from: Timestamp | null
  to: Timestamp | null
  expiresAt: Timestamp | null
  lastViewedAt: Timestamp | null
  viewCount: number
  active: boolean
  createdAt: Timestamp
}

/**
 * Everything a shared page shows. The shape is the privacy boundary: there is
 * no listening history here and no way to ask for one.
 */
export interface SharedStats {
  label: string
  displayName: string
  avatarUrl: string
  timezone: string
  rolling: boolean
  rangeDays: number
  from: Timestamp
  to: Timestamp
  interval: Interval
  summary: Summary
  tracks: Page<TopEntry<TrackRef>>
  artists: Page<TopEntry<ArtistRef>>
  albums: Page<TopEntry<AlbumRef>>
  timeline: TimelineBucket[]
  hours: RepartitionBucket[]
  weekdays: RepartitionBucket[]
  /**
   * Aggregate taste — the same data class as the top lists above: what
   * somebody listens to, never how or where. Optional because the Go DTO
   * carries both as `omitempty` pointers; playback-context statistics (skip,
   * shuffle, platform, country, offline, incognito) are never in this shape,
   * on any version, because device and location are not what a share is for.
   */
  genres?: GenresResponse
  taste?: TasteResponse
}

// --- playlists -------------------------------------------------------------

/** How a playlist definition chooses its tracks. */
export type PlaylistMode = 'top' | 'min_plays' | 'discoveries' | 'forgotten'

/** What a mode ranks by. */
export type PlaylistSort = 'plays' | 'time'

export interface CreatePlaylistRequest {
  name: string
  mode: PlaylistMode
  sort?: PlaylistSort
  limit?: number
  minPlays?: number
  from?: Timestamp
  to?: Timestamp
}

/** One track a definition selected, and why. */
export interface PlaylistTrack {
  rank: number
  track: TrackRef
  plays: number
  msPlayed: number
}

/** What a definition would produce, without producing it. */
export interface PlaylistPreview {
  tracks: PlaylistTrack[]
  /** How many qualified before the limit was applied. */
  matched: number
  /** What the limit was. */
  limit: number
}

/** The body of PATCH /api/playlists/{id}. */
export interface RenamePlaylistRequest {
  name: string
}

/**
 * What happened the last time Encore tried to give a playlist a picture.
 *
 * `unauthorised` is separate from `failed` because the fix is a consent journey
 * rather than a retry, and the two must not share a button.
 */
export interface PlaylistCover {
  state: 'none' | 'ready' | 'failed' | 'unauthorised'
  /** `mosaic` or `pattern`. Empty unless `state` is `ready`. */
  kind: string
  /** How many of `total` tiles came from real album artwork. */
  covered: number
  total: number
  /** Why the last attempt failed. Empty unless `state` is `failed`. */
  reason: string
  /** When `state` was last written. Null while it is `none`. */
  at: Timestamp | null
}

export interface Playlist {
  id: string
  name: string
  spotifyId: string
  spotifyUrl: string
  mode: PlaylistMode
  sort: PlaylistSort
  limit: number
  minPlays: number
  from: Timestamp | null
  to: Timestamp | null
  trackCount: number
  /** How many tracks qualified before the limit. Only on a build response. */
  matched?: number
  builtAt: Timestamp | null
  cover: PlaylistCover
  createdAt: Timestamp
}

// --- instance status -------------------------------------------------------

/**
 * How far enrichment has got with one kind of catalogue entity.
 *
 * `named` is tracked apart from `resolved` because the two genuinely differ: an
 * imported track carries its title in the export, so it is readable long before
 * Spotify has supplied its album, artwork and duration. Showing only `resolved`
 * would report everything as missing when most of what is on screen is already
 * there.
 */
export interface EntityProgress {
  total: number
  resolved: number
  pending: number
  failed: number
  unavailable: number
  named: number
  /**
   * Rows an import named but could not identify. The exports give an artist and
   * an album for every play and an id for neither, so these are readable but
   * carry no artwork or genres, and no queue can fetch them.
   */
  local: number
}

export interface CatalogueProgress {
  tracks: EntityProgress
  artists: EntityProgress
  albums: EntityProgress
  aliasesTotal: number
  aliasesPending: number
}

/** Whether metadata is still arriving, and whether anything is stopping it. */
export interface MetadataStatus {
  outstanding: number
  complete: boolean
  /** True when Spotify has rate limited this whole instance. */
  paused: boolean
  /** When enrichment resumes. Null unless `paused`. */
  pausedUntil: Timestamp | null
  /**
   * A second metadata source is configured, so a pause slows enrichment down
   * rather than stopping it. Reported from the API's own environment, which
   * means the deployment is set up for one — not that the worker has reached it.
   */
  fallbackConfigured: boolean
}

export interface StatusResponse {
  catalogue: CatalogueProgress
  metadata: MetadataStatus
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

// --- genres ------------------------------------------------------------

/** The denominator every partial statistic carries. */
export interface Coverage {
  covered: number
  total: number
}

/** A ratio and the coverage it was computed over. */
export interface Rate extends Coverage {
  value: number
}

export interface GenreEntry {
  genre: string
  plays: number
  msPlayed: number
}

/**
 * One page of the genre ranking.
 *
 * Plays across genres sum to more than the range's total plays: a track counts
 * toward each of its genres. The page says so rather than normalising it away.
 */
export interface GenresResponse {
  genres: GenreEntry[]
  total: number
  coverage: Coverage
}

export interface GenreTimelinePoint {
  bucket: string
  genre: string
  plays: number
  msPlayed: number
}

export interface GenreTimelineResponse {
  interval: Interval
  points: GenreTimelinePoint[]
}

// --- habits (playback context & taste) --------------------------------------

/** One category of a breakdown: an end reason, a platform family, a country. */
export interface ContextSlice {
  key: string
  plays: number
}

/** The two taste scores, each with its own coverage. Consumed by the dashboard. */
export interface TasteResponse {
  obscurity: Rate
  releaseLag: Rate
}

/**
 * One (contextType, contextId) group: what the listener was playing from —
 * a playlist, an album, an artist, or "collection" (Spotify's own encoding for
 * Liked Songs) — and how many times.
 *
 * `name` is empty whenever `user_playlists` has no match for the pair: every
 * album, artist and collection context always, and a playlist context whenever
 * its id no longer names one of the listener's own playlists (deleted since,
 * or never theirs to begin with). An empty name is not an error state — the
 * row is still real and still counted — so the page must render something
 * meaningful from `contextType` rather than dropping it or showing the raw id.
 */
export interface PlaylistContextEntry {
  contextType: string
  contextId: string
  name: string
  plays: number
}

/**
 * How listening happened, as opposed to what was listened to.
 *
 * Every rate carries its own denominator: the underlying columns are written
 * only by the extended-export importer, and an export may omit any one of them
 * independently of the others. `playlists`/`playlistCoverage` are the one
 * exception to that lineage — `context_type`/`context_id` are written only by
 * live sync, never by any Spotify export — so that coverage is independent of,
 * and typically disjoint from, the other six figures here.
 */
export interface PlaybackContextResponse {
  endReasons: ContextSlice[]
  endReasonCoverage: Coverage
  skipRate: Rate
  shuffleRate: Rate
  platforms: ContextSlice[]
  platformCoverage: Coverage
  countries: ContextSlice[]
  countryCoverage: Coverage
  offlineRate: Rate
  incognitoRate: Rate
  playlists: PlaylistContextEntry[]
  playlistCoverage: Coverage
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

/**
 * Range-scoped: both numbers describe albums played inside the selected range,
 * counting only those whose track count is known. The aggregate names its own
 * denominator in prose rather than shipping a bare count, because — unlike
 * every other figure on the dashboard — it is not the same population as the
 * album page's all-time `AlbumCompletion` beside it.
 */
export interface CompletedAlbums {
  complete: number
  albums: number
}

export interface StatsExtras {
  differentArtists: number
  averageAlbumReleaseYear: number | null
  averageArtistsPerTrack: number
  albumsCompleted?: CompletedAlbums
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

// --- library -----------------------------------------------------------

/**
 * One saved track nothing in the fact table has ever played. All time
 * regardless of the selected range: see `LibraryStatsResponse` for why
 * narrowing the window must not change this list.
 *
 * `addedAt` is null when Spotify did not report it, or the listener saved
 * the track before Encore recorded that field.
 */
export interface LibrarySavedTrack {
  entity: TrackRef
  addedAt: Timestamp | null
}

/** One track played inside the range that the caller has never saved. */
export interface LibraryPlayedTrack {
  entity: TrackRef
  plays: number
  msPlayed: number
}

/**
 * One followed artist with no play inside the range. `lastPlayedAt` is the
 * artist's last play ever, regardless of the range, so the page can say how
 * long it has actually been rather than only that it has been a while; null
 * when the artist has never been played at all.
 */
export interface LibraryDormantArtist {
  entity: ArtistRef
  lastPlayedAt: Timestamp | null
}

/**
 * Answers `GET /api/stats/library`: the last enumeration's snapshot, plus
 * the three lists that cross what has been saved and followed against what
 * has actually been played.
 *
 * `syncedAt` is null until the library worker's first successful run — the
 * state of every account on an upgraded instance — and must never be treated
 * as a zero timestamp: it is how the page tells "never enumerated" apart
 * from "enumerated and found nothing".
 *
 * `savedNeverPlayed` is all time regardless of the requested range.
 * `playedNeverSaved` and `dormantFollows` both follow it. `syncedAt` and the
 * three counts describe neither: they are a snapshot of the last
 * enumeration itself.
 */
export interface LibraryStatsResponse {
  syncedAt: Timestamp | null
  savedTracks: number
  savedAlbums: number
  followedArtists: number
  savedNeverPlayed: LibrarySavedTrack[]
  playedNeverSaved: LibraryPlayedTrack[]
  dormantFollows: LibraryDormantArtist[]
}

// --- top diff ------------------------------------------------------------

/** The two entity kinds Spotify's own top-items endpoint knows. Spotify has no top-albums endpoint. */
export type TopDiffKind = 'track' | 'artist'

/**
 * Spotify's own three rolling top-items windows. See `TopDiffResponse` for
 * why this is the only "range" the top-diff page has — there is no `from`/
 * `to` picker here.
 */
export type SpotifyTimeRange = 'short_term' | 'medium_term' | 'long_term'

/**
 * One entity in the comparison between Spotify's own top ranking and
 * Encore's, for one `(kind, range)` pair.
 *
 * `spotifyRank` and `encoreRank` are null exactly when the entity is absent
 * from that side, never zero — an entity present on only one side is the
 * disagreement this comparison exists to surface. `plays` is Encore's own
 * play count for the window; Spotify's side carries no play count at all,
 * only a rank, so `plays` is meaningless when `encoreRank` is null.
 */
export interface TopDiffEntry<T> {
  entity: T
  spotifyRank: number | null
  encoreRank: number | null
  plays: number
}

/**
 * Answers `GET /api/stats/top-diff`.
 *
 * `capturedAt` is null exactly when nothing has ever been captured for this
 * `(kind, range)` set — an account without `user-top-read`, or one whose
 * daily capture has not run since they granted it — and `entries` is then
 * always `[]`, never Encore's ranking shown on its own.
 */
export interface TopDiffResponse<T> {
  capturedAt: Timestamp | null
  timeRange: SpotifyTimeRange
  entries: TopDiffEntry<T>[]
}

/**
 * One track, artist or album on its detail page.
 *
 * `plays` and `msPlayed` are scoped to the selected range. The timestamps come
 * in two pairs and mean different things:
 *
 * - `firstListenAt` / `lastListenAt` — the first and last play **inside the
 *   range**.
 * - `discoveredAt` / `lastPlayedAt` — the first and last play **ever**,
 *   ignoring the range entirely.
 *
 * Anything labelled "first listen" wants the second pair. The first pair
 * describes the window, not the music.
 */
export interface EntityStats {
  plays: number
  msPlayed: number
  firstListenAt: Timestamp | null
  lastListenAt: Timestamp | null
  discoveredAt: Timestamp | null
  lastPlayedAt: Timestamp | null
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

/**
 * How much of an album has been heard, ever — not scoped to the selected
 * range, unlike everything else on the album page. `discoveredAt` and
 * `lastPlayedAt` on `EntityStats` above carry the same all-time property for
 * the same reason: a range re-derives "first heard" or "how much of this
 * record you know" into a fact about the window rather than about the music.
 */
export interface AlbumCompletion {
  heard: number
  total: number
  /** False when the album's track count has not been enriched yet. */
  known: boolean
}

export interface AlbumDetail {
  album: Album
  stats: EntityStats
  topTracks: TopEntry<TrackRef>[]
  completion?: AlbumCompletion
}

/**
 * Which tracks of an album have never been played.
 *
 * Unlike everything else on the album page this is not computed from data on
 * disk: Encore has to ask Spotify what the album contains, which it does the
 * first time somebody opens the page and then caches. So `missing` being empty
 * is ambiguous, and `state` is what resolves it — an empty `missing` under
 * anything but `ready` means "Encore does not know", never "you have played
 * everything".
 *
 * `disabled` is the instance's operator having turned fetching off
 * (`ENCORE_ALBUM_TRACKS_ENABLED=false`). It is separate from `unavailable` on
 * purpose: one is a local choice and the other is Spotify failing to answer,
 * and rendering the second for the first blames a third party for a decision
 * somebody here made. A listing cached before the switch was flipped still
 * arrives as `ready`.
 *
 * `pending` carries no clock and nothing bounds it: every `pending` response
 * for one album is byte-identical however long the state has held, and the
 * server's own doc says a claim that errored re-enters it on the very next
 * request. A client that polls this state must therefore cap itself; see
 * `tracklistPollInterval` on the album page.
 */
export type AlbumTrackListState = LazyFetchState

export interface AlbumTrackRef {
  id: string
  name: string
  discNumber: number
  trackNumber: number
}

export interface AlbumTrackList {
  state: AlbumTrackListState
  /**
   * Played over listed. The denominator is the listing Spotify returned, which
   * is not necessarily `album.totalTracks`: they come from different reads and
   * can disagree, so the panel says which one it followed.
   */
  coverage: Coverage
  missing: AlbumTrackRef[]
  /**
   * When the listing was read. Absent until one has succeeded.
   *
   * The panel renders it on every `ready` state. It is the only thing keeping a
   * cached listing honest on an instance where fetching has been turned off and
   * no refresh is ever coming — a date says how old an answer is without
   * claiming anything about how current it is.
   */
  fetchedAt?: Timestamp
}

export interface DiscographyAlbumRef {
  id: string
  name: string
  /** "2016", "2016-05" or "2016-05-20", at whatever precision Spotify knew. */
  releaseDate: string | null
  releasePrecision: string
}

/**
 * What discography coverage did *not* count.
 *
 * Coverage counts `album_group = "album"` only, because "you have heard 4 of 340
 * releases" is not a useful sentence. That makes coverage alone an overclaim by
 * omission, so **a panel that renders the coverage must also name these**: an
 * artist with 340 singles otherwise looks like an artist with 11 releases.
 *
 * `other` is any group Spotify sends that is none of the four it documents. It
 * is zero today and exists so the four buckets plus `coverage.total` always
 * account for every release stored, rather than a new group vanishing from both
 * the count and the sentence describing the rest.
 */
export interface DiscographyExcluded {
  singles: number
  compilations: number
  appearsOn: number
  other: number
}

/**
 * How much of an artist's own catalogue has been played.
 *
 * Not computed from data on disk: Encore has to ask Spotify what the artist
 * released, which it does the first time somebody opens the page and then
 * caches. So `missing` being empty is ambiguous, and `state` resolves it — an
 * empty `missing` under anything but `ready` means "Encore does not know", never
 * "you have played everything".
 *
 * `disabled` is the operator having turned fetching off
 * (`ENCORE_ARTIST_ALBUMS_ENABLED=false`), separate from `unavailable` for the
 * reason it is on the album tracklist: one is a local choice and the other is
 * Spotify failing to answer.
 *
 * Two things have no counterpart on the album page. `ready` with
 * `coverage.total === 0` is a real answer — an artist whose every release is a
 * single — and `excluded` is then the only thing that describes them. And
 * `covered` counts albums with *any* play, not albums played in full, so a panel
 * must say so or "4 of 11 albums" reads as four albums heard end to end.
 *
 * `pending` carries no clock and nothing bounds it, exactly as on the album
 * tracklist; a client that polls it must cap itself.
 */
export interface ArtistDiscography {
  state: LazyFetchState
  /** Albums with any play, over albums Spotify lists in the `album` group. */
  coverage: Coverage
  /** The counted albums with no play, newest release first. Never null. */
  missing: DiscographyAlbumRef[]
  excluded: DiscographyExcluded
  /** When the discography was read. Absent until one has succeeded. */
  fetchedAt?: Timestamp
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
