/**
 * Settings: the account, the instrument's calibration, and the way out.
 *
 * The order is deliberate. The Spotify link comes first because a lapsed grant
 * silently stops new listens arriving and is the one setting that breaks
 * something. The timezone is next because it changes what every statistic in
 * Encore means. Deleting the account is last, behind a disclosure and a typed
 * confirmation, because it is irreversible and nobody should reach it by
 * mis-clicking a button they were scrolling past.
 */

import type { ReactElement, ReactNode } from 'react'
import { useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import { qk } from '../lib/query'
import { useSession } from '../lib/session'
import { THEME_MODES, themeLabel, useTheme } from '../lib/theme'
import type { ThemeMode } from '../lib/theme'
import {
  EMPTY,
  formatCount,
  formatDateTime,
  formatPercent,
  formatPlural,
  formatRelative,
} from '../lib/format'
import type {
  Artist,
  CreatePlaylistRequest,
  CreateShareRequest,
  EntityProgress,
  Playlist,
  PlaylistMode,
  PlaylistPreview,
  ShareLink,
  StatusResponse,
  SyncOutcome,
  User,
} from '../lib/types'
import {
  Button,
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  Field,
  Icon,
  Input,
  Panel,
  PageHeader,
  Select,
  Skeleton,
  buttonClass,
  errorMessage,
  useToast,
} from '../components/ui'

/**
 * How often the metadata panel re-reads the instance status while work is
 * outstanding. Enrichment commits in batches, so a slower poll than this would
 * make a moving number look stuck, and a faster one would show the same figure
 * twice.
 */
const STATUS_POLL_MS = 30_000

/** What each mode does, in the words the form uses. */
const MODE_HINTS: Record<PlaylistMode, string> = {
  top: 'The most played tracks of the period.',
  min_plays: 'Everything that reached a play count, however many that is.',
  discoveries: 'Tracks you heard for the first time ever in the period.',
  forgotten: 'Played heavily before the period, and not during it.',
}

/** The same modes, shortened for a list row. */
const MODE_LABELS: Record<PlaylistMode, string> = {
  top: 'most played',
  min_plays: 'by play count',
  discoveries: 'discoveries',
  forgotten: 'forgotten favourites',
}

/**
 * Every timezone this browser knows, with the configured one guaranteed to be
 * in the list. A browser too old to enumerate them still gets a working control
 * rather than an empty one — it just offers fewer choices.
 */
function supportedTimeZones(current: string): string[] {
  let zones: string[] = []
  try {
    if (typeof Intl.supportedValuesOf === 'function') zones = Intl.supportedValuesOf('timeZone')
  } catch {
    zones = []
  }
  if (zones.length === 0) {
    let local = 'UTC'
    try {
      local = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
    } catch {
      local = 'UTC'
    }
    zones = [local, 'UTC']
  }
  return [...new Set([current, ...zones])].filter(Boolean).sort((a, b) => a.localeCompare(b))
}

/** What a manual sync found, said so that "nothing new" reads as a normal result. */
function syncSentence(outcome: SyncOutcome): string {
  if (outcome.fetched === 0) {
    return 'Spotify’s recent feed was empty, so there was nothing to add.'
  }
  if (outcome.imported === 0) {
    return `Checked ${formatPlural(outcome.fetched, 'play')} and found nothing new. Spotify’s feed only reaches back fifty plays, so this is the usual result.`
  }
  return `Added ${formatPlural(outcome.imported, 'listen')} from ${formatPlural(outcome.fetched, 'play')} checked.`
}

export default function Settings(): ReactElement {
  const { user, spotify, listening, isAdmin, refresh, logout } = useSession()
  const { mode, setMode } = useTheme()
  const queryClient = useQueryClient()
  const toast = useToast()
  const navigate = useNavigate()

  const [zoneDraft, setZoneDraft] = useState<string | null>(null)
  const [syncNote, setSyncNote] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [confirmName, setConfirmName] = useState('')

  const playlistYears = useMemo(() => {
    const first = listening?.firstListenAt
      ? new Date(listening.firstListenAt).getUTCFullYear()
      : null
    const last = listening?.lastListenAt ? new Date(listening.lastListenAt).getUTCFullYear() : null
    if (first === null || last === null) return []
    const years: number[] = []
    for (let year = last; year >= first; year -= 1) years.push(year)
    return years
  }, [listening])

  const currentZone = user?.timezone ?? 'UTC'
  const zones = useMemo(() => supportedTimeZones(currentZone), [currentZone])
  const zoneGroups = useMemo(() => {
    const map = new Map<string, string[]>()
    for (const zone of zones) {
      const region = zone.includes('/') ? zone.slice(0, zone.indexOf('/')) : 'Other'
      const list = map.get(region)
      if (list) list.push(zone)
      else map.set(region, [zone])
    }
    return [...map.entries()]
  }, [zones])

  const blacklist = useQuery({
    queryKey: qk.blacklist(),
    queryFn: ({ signal }) => api.get<Artist[]>('/blacklist', undefined, signal),
  })

  const shares = useQuery({
    queryKey: qk.shares(),
    queryFn: ({ signal }) => api.get<ShareLink[]>('/shares', undefined, signal),
  })

  // The token comes back once, in the creating response. It is held here so the
  // owner can copy it, and it is gone as soon as the page is left — which is
  // also true of the server, where only its hash was ever stored.
  const [freshLink, setFreshLink] = useState<ShareLink | null>(null)
  const [shareLabel, setShareLabel] = useState('')
  const [shareWindow, setShareWindow] = useState('all')

  const createShare = useMutation({
    mutationFn: (body: CreateShareRequest) => api.post<ShareLink>('/shares', body),
    onSuccess: (link) => {
      setFreshLink(link)
      setShareLabel('')
      void queryClient.invalidateQueries({ queryKey: qk.shares() })
    },
  })

  const revokeShare = useMutation({
    mutationFn: (id: string) => api.del<void>(`/shares/${id}`),
    onSuccess: (_result, id) => {
      setFreshLink((current) => (current?.id === id ? null : current))
      void queryClient.invalidateQueries({ queryKey: qk.shares() })
      toast.notify({ tone: 'success', title: 'Link revoked', description: 'It stops working now.' })
    },
  })

  const playlists = useQuery({
    queryKey: qk.playlists(),
    queryFn: ({ signal }) => api.get<Playlist[]>('/playlists', undefined, signal),
  })

  const [playlistName, setPlaylistName] = useState('')
  const [playlistMode, setPlaylistMode] = useState<PlaylistMode>('top')
  const [playlistLimit, setPlaylistLimit] = useState('100')
  const [playlistMinPlays, setPlaylistMinPlays] = useState('10')
  const [playlistYear, setPlaylistYear] = useState('')

  // Held rather than cached: a preview describes the form as it was when the
  // button was pressed, and silently refreshing it under an edited form would
  // show somebody a list that answers a different question.
  const [preview, setPreview] = useState<PlaylistPreview | null>(null)

  // Built once so preview and create ask exactly the same question. Two copies
  // of this would eventually differ, and the preview would stop being one.
  const playlistBody = (): CreatePlaylistRequest => {
    const body: CreatePlaylistRequest = {
      name: playlistName.trim(),
      mode: playlistMode,
      limit: Number(playlistLimit) || 100,
    }
    if (playlistMode === 'min_plays') body.minPlays = Number(playlistMinPlays) || 10
    if (playlistYear !== '') {
      const year = Number(playlistYear)
      body.from = new Date(Date.UTC(year, 0, 1)).toISOString()
      body.to = new Date(Date.UTC(year + 1, 0, 1)).toISOString()
    }
    return body
  }

  const previewPlaylist = useMutation({
    mutationFn: (body: CreatePlaylistRequest) =>
      api.post<PlaylistPreview>('/playlists/preview', body),
    onSuccess: setPreview,
  })

  const createPlaylist = useMutation({
    mutationFn: (body: CreatePlaylistRequest) => api.post<Playlist>('/playlists', body),
    onSuccess: (made) => {
      setPlaylistName('')
      setPreview(null)
      void queryClient.invalidateQueries({ queryKey: qk.playlists() })
      toast.notify({
        tone: 'success',
        title: `${made.name} created`,
        description:
          made.matched && made.matched > made.trackCount
            ? `${formatCount(made.trackCount)} of ${formatCount(made.matched)} matching tracks.`
            : `${formatPlural(made.trackCount, 'track')}.`,
      })
    },
  })

  const rebuildPlaylist = useMutation({
    mutationFn: (id: string) => api.post<Playlist>(`/playlists/${id}/rebuild`),
    onSuccess: (made) => {
      void queryClient.invalidateQueries({ queryKey: qk.playlists() })
      toast.notify({
        tone: 'success',
        title: `${made.name} rebuilt`,
        description: `${formatPlural(made.trackCount, 'track')}.`,
      })
    },
  })

  const forgetPlaylist = useMutation({
    mutationFn: (id: string) => api.del<void>(`/playlists/${id}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: qk.playlists() })
      toast.notify({
        tone: 'success',
        title: 'Encore stopped managing it',
        description: 'The playlist itself is still in your Spotify library.',
      })
    },
  })

  const status = useQuery({
    queryKey: qk.status(),
    queryFn: ({ signal }) => api.get<StatusResponse>('/status', undefined, signal),
    // Enrichment moves on its own while this page is open, so the panel keeps
    // itself current until there is nothing left to fetch. Once the queues are
    // empty and Spotify is not holding us back, the numbers cannot change
    // without the user doing something, and polling would be pure server load.
    refetchInterval: (query) => {
      const data = query.state.data
      if (!data) return false
      return data.metadata.complete && !data.metadata.paused ? false : STATUS_POLL_MS
    },
  })

  const sync = useMutation({
    mutationFn: () => api.post<SyncOutcome>('/sync/now'),
    onSuccess: async (outcome) => {
      setSyncNote(
        outcome.newestAt
          ? `${syncSentence(outcome)} Newest play ${formatRelative(outcome.newestAt)}.`
          : syncSentence(outcome),
      )
      await refresh()
      void queryClient.invalidateQueries({ queryKey: qk.stats() })
      void queryClient.invalidateQueries({ queryKey: ['history'] })
    },
  })

  const saveZone = useMutation({
    mutationFn: (timezone: string) => api.patch<User>('/me', { timezone }),
    onSuccess: async (updated) => {
      setZoneDraft(null)
      await refresh()
      // Every figure in Encore is bucketed in this zone on the server, so the
      // whole cache is now describing the old one.
      void queryClient.invalidateQueries()
      toast.notify({
        tone: 'success',
        title: 'Timezone saved',
        description: `Statistics are bucketed in ${updated.timezone} from now on.`,
      })
    },
  })

  const unhide = useMutation({
    mutationFn: (artistId: string) => api.del<void>(`/blacklist/${artistId}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: qk.blacklist() })
      void queryClient.invalidateQueries({ queryKey: qk.stats() })
      toast.notify({ tone: 'success', title: 'Artist shown again' })
    },
  })

  const deleteAccount = useMutation({
    mutationFn: (confirm: string) => api.del<void>('/me', { confirm }),
    onSuccess: async () => {
      await logout()
      void navigate('/login', { replace: true })
    },
  })

  if (!user) {
    return (
      <div className="space-y-5">
        <PageHeader title="Settings" description="Your account and this instance." />
        <Panel padded={false}>
          <EmptyState
            icon="settings"
            title="You are not signed in"
            description="Sign in with Spotify to see your settings."
            action={
              <a className={buttonClass('primary')} href="/api/auth/spotify/login">
                Sign in with Spotify
              </a>
            }
          />
        </Panel>
      </div>
    )
  }

  const zone = zoneDraft ?? user.timezone
  const zoneDirty = zone !== user.timezone
  const needsReauth = spotify?.syncState === 'needs_reauth'
  const syncFailed = spotify?.syncState === 'error'
  const canSync = Boolean(spotify?.connected) && !needsReauth
  // Encore signs everybody in read-only. Write access is granted separately, by
  // whoever actually wants a playlist made.
  const canWritePlaylists = (spotify?.scopes ?? []).some((granted) =>
    granted.split(' ').includes('playlist-modify-private'),
  )
  const confirmMatches = confirmName.trim() === user.spotifyUserId

  return (
    <div className="space-y-5">
      <PageHeader
        title="Settings"
        description="Your Spotify link, how time is counted, and what happens to your data."
        actions={
          isAdmin ? (
            <ButtonLink to="/settings/admin">
              <Icon name="admin" />
              Administration
            </ButtonLink>
          ) : null
        }
      />

      <div className="grid gap-5 lg:grid-cols-2">
        {/* --- Spotify ------------------------------------------------------ */}
        <Panel
          title="Spotify"
          description="Where new listens come from."
          actions={
            needsReauth ? (
              <Chip tone="bad">Reconnect needed</Chip>
            ) : syncFailed ? (
              <Chip tone="warn">Sync error</Chip>
            ) : spotify?.connected ? (
              <Chip tone="good">Connected</Chip>
            ) : (
              <Chip>Not connected</Chip>
            )
          }
        >
          <dl className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
            <Detail label="Account" value={user.spotifyUserId} />
            <Detail
              label="Last sync"
              value={
                spotify?.lastSyncAt
                  ? `${formatDateTime(spotify.lastSyncAt, user.timezone)} (${formatRelative(spotify.lastSyncAt)})`
                  : 'Never'
              }
            />
          </dl>

          {spotify?.lastSyncError ? (
            <p className="mt-3 text-sm text-ink-muted">
              <span className="eyebrow mr-2">Last error</span>
              {spotify.lastSyncError}
            </p>
          ) : null}

          {needsReauth ? (
            <div className="mt-4 border-t border-seam pt-4">
              <p className="max-w-prose text-sm text-ink">
                The link to Spotify has expired, so new plays are no longer arriving. Reconnecting
                loses no history.
              </p>
              <a
                // A full navigation, not a fetch: the server answers with a
                // redirect to Spotify's authorisation page.
                className={`${buttonClass('primary')} mt-3`}
                href="/api/auth/spotify/relink"
              >
                <Icon name="refresh" />
                Reconnect Spotify
              </a>
            </div>
          ) : null}

          <div className="mt-4 border-t border-seam pt-4">
            <div className="flex flex-wrap items-center gap-3">
              <Button
                busy={sync.isPending}
                disabled={!canSync}
                onClick={() => {
                  setSyncNote('')
                  sync.mutate()
                }}
              >
                Sync now
              </Button>
              <p className="text-xs text-ink-faint">
                Encore syncs on its own schedule; this asks Spotify straight away.
              </p>
            </div>
            <p role="status" aria-live="polite" className="mt-2 min-h-5 text-sm text-ink-muted">
              {sync.isPending ? 'Asking Spotify for your recent plays…' : syncNote}
            </p>
            {sync.isError ? (
              <p role="alert" className="text-sm text-ember">
                {errorMessage(sync.error)}
              </p>
            ) : null}
            {!canSync ? (
              <p className="text-sm text-ink-muted">
                Reconnect Spotify before syncing: without a valid grant there is nothing to ask.
              </p>
            ) : null}
          </div>
        </Panel>

        {/* --- metadata ----------------------------------------------------- */}
        <MetadataPanel status={status.data} error={status.error} timezone={user.timezone} />

        {/* --- timezone ----------------------------------------------------- */}
        <Panel title="Timezone" description="The clock every statistic is counted against.">
          <form
            onSubmit={(event) => {
              event.preventDefault()
              if (zoneDirty) saveZone.mutate(zone)
            }}
          >
            <Field
              label="Your timezone"
              hint="Dates and times everywhere in Encore are shown in this zone."
            >
              <Select value={zone} onChange={(event) => setZoneDraft(event.target.value)}>
                {zoneGroups.map(([region, list]) => (
                  <optgroup key={region} label={region}>
                    {list.map((name) => (
                      <option key={name} value={name}>
                        {name.replace(/_/g, ' ')}
                      </option>
                    ))}
                  </optgroup>
                ))}
              </Select>
            </Field>

            <p className="mt-3 max-w-prose text-sm text-ink-muted">
              Every figure is bucketed in this zone. Changing it re-buckets your history
              automatically — nothing is lost.
            </p>

            <div className="mt-3 flex flex-wrap items-center gap-3">
              <Button
                type="submit"
                variant="primary"
                busy={saveZone.isPending}
                disabled={!zoneDirty}
              >
                Save timezone
              </Button>
              {zoneDirty ? (
                <Button onClick={() => setZoneDraft(null)} disabled={saveZone.isPending}>
                  Cancel
                </Button>
              ) : null}
            </div>
            {saveZone.isError ? (
              <p role="alert" className="mt-2 text-sm text-ember">
                {errorMessage(saveZone.error)}
              </p>
            ) : null}
          </form>
        </Panel>

        {/* --- theme -------------------------------------------------------- */}
        <Panel title="Appearance" description="Light, dark, or whatever the machine is doing.">
          <fieldset>
            <legend className="eyebrow">Theme</legend>
            <div className="mt-2 flex flex-wrap gap-4">
              {THEME_MODES.map((option: ThemeMode) => (
                <label key={option} className="flex items-center gap-2 text-sm text-ink">
                  <input
                    type="radio"
                    name="theme"
                    value={option}
                    checked={mode === option}
                    onChange={() => setMode(option)}
                    className="h-4 w-4 accent-lamp"
                  />
                  {themeLabel(option)}
                </label>
              ))}
            </div>
          </fieldset>
          <p className="mt-3 text-sm text-ink-muted">
            System follows your operating system and changes with it. The choice is stored in this
            browser, so each device can differ. The control in the top bar cycles the same setting.
          </p>
        </Panel>

        {/* --- export ------------------------------------------------------- */}
        <Panel title="Your data" description="Everything Encore holds about your listening.">
          <p className="max-w-prose text-sm text-ink-muted">
            Every play, with its track, artists, album and the time it happened. JSON keeps the
            structure; CSV opens in a spreadsheet.
          </p>
          <div className="mt-3 flex flex-wrap gap-2">
            <a className={buttonClass()} href="/api/me/export?format=json">
              Download JSON
            </a>
            <a className={buttonClass()} href="/api/me/export?format=csv">
              Download CSV
            </a>
          </div>
        </Panel>
      </div>

      {/* --- playlists ------------------------------------------------------ */}
      <Panel
        title="Playlists"
        description="Build a Spotify playlist from what you actually listened to."
      >
        <form
          onSubmit={(event) => {
            event.preventDefault()
            createPlaylist.mutate(playlistBody())
          }}
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Name" hint="What it will be called in Spotify.">
              <Input
                value={playlistName}
                maxLength={100}
                required
                placeholder="Heavy rotation"
                onChange={(event) => setPlaylistName(event.target.value)}
              />
            </Field>
            <Field label="Choose tracks by" hint={MODE_HINTS[playlistMode]}>
              <Select
                value={playlistMode}
                onChange={(event) => setPlaylistMode(event.target.value as PlaylistMode)}
              >
                <option value="top">Most played</option>
                <option value="min_plays">Played at least N times</option>
                <option value="discoveries">First heard in the period</option>
                <option value="forgotten">Forgotten favourites</option>
              </Select>
            </Field>
            <Field
              label="Period"
              hint={
                playlistMode === 'forgotten'
                  ? 'Required: the period they dropped out of.'
                  : 'Leave on everything for your whole history.'
              }
            >
              <Select
                value={playlistYear}
                onChange={(event) => setPlaylistYear(event.target.value)}
              >
                <option value="">Everything</option>
                {playlistYears.map((year) => (
                  <option key={year} value={String(year)}>
                    {year}
                  </option>
                ))}
              </Select>
            </Field>
            {playlistMode === 'min_plays' ? (
              <Field label="Minimum plays" hint="Every track that reached this count.">
                <Input
                  type="number"
                  min={1}
                  max={10000}
                  value={playlistMinPlays}
                  onChange={(event) => setPlaylistMinPlays(event.target.value)}
                />
              </Field>
            ) : (
              <Field label="How many tracks" hint="At most 500.">
                <Input
                  type="number"
                  min={1}
                  max={500}
                  value={playlistLimit}
                  onChange={(event) => setPlaylistLimit(event.target.value)}
                />
              </Field>
            )}
          </div>

          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Button
              busy={previewPlaylist.isPending}
              onClick={() => previewPlaylist.mutate(playlistBody())}
            >
              Preview
            </Button>
            {canWritePlaylists ? (
              <Button
                type="submit"
                variant="primary"
                busy={createPlaylist.isPending}
                disabled={playlistName.trim() === ''}
              >
                Create in Spotify
              </Button>
            ) : (
              <a className={buttonClass('primary')} href="/api/auth/spotify/playlists">
                <Icon name="refresh" />
                Allow Encore to create playlists
              </a>
            )}
          </div>

          {!canWritePlaylists ? (
            <p className="mt-2 max-w-prose text-sm text-ink-muted">
              Previewing reads only your own listening. Creating the playlist needs permission — one
              trip through Spotify, revocable from your account at any time.
            </p>
          ) : null}

          {previewPlaylist.isError ? (
            <p role="alert" className="mt-2 text-sm text-ember">
              {errorMessage(previewPlaylist.error)}
            </p>
          ) : null}
          {createPlaylist.isError ? (
            <p role="alert" className="mt-2 text-sm text-ember">
              {errorMessage(createPlaylist.error)}
            </p>
          ) : null}

          {preview ? (
            <div className="mt-4 border-t border-seam pt-4">
              <p className="text-sm text-ink">
                {preview.tracks.length === 0
                  ? 'Nothing matches that description.'
                  : preview.matched > preview.tracks.length
                    ? `${formatPlural(preview.tracks.length, 'track')}, of ${formatCount(
                        preview.matched,
                      )} that qualified. Raise the limit to take more.`
                    : `${formatPlural(preview.tracks.length, 'track')} — everything that qualified.`}
              </p>
              {preview.tracks.length > 0 ? (
                <ol className="mt-2 max-h-80 divide-y divide-seam overflow-y-auto">
                  {preview.tracks.map((entry) => (
                    <li key={entry.track.id} className="flex items-baseline gap-3 py-1.5 text-sm">
                      <span className="tabular w-8 shrink-0 text-right text-xs text-ink-faint">
                        {entry.rank}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span className="text-ink">{entry.track.name || 'Not yet named'}</span>
                        {entry.track.artists.length > 0 ? (
                          <span className="block text-xs text-ink-muted">
                            {entry.track.artists.map((a) => a.name).join(', ')}
                          </span>
                        ) : null}
                      </span>
                      <span className="tabular shrink-0 text-xs text-ink-faint">
                        {formatPlural(entry.plays, 'play')}
                      </span>
                    </li>
                  ))}
                </ol>
              ) : null}
            </div>
          ) : null}
        </form>

        {(playlists.data?.length ?? 0) === 0 ? null : (
          <ul className="mt-4 divide-y divide-seam border-t border-seam">
            {(playlists.data ?? []).map((playlist) => (
              <li key={playlist.id} className="flex flex-wrap items-center gap-3 py-2.5">
                <div className="min-w-0 flex-1">
                  <a
                    className="text-sm text-ink hover:text-lamp"
                    href={playlist.spotifyUrl}
                    target="_blank"
                    rel="noreferrer noopener"
                  >
                    {playlist.name}
                  </a>
                  <p className="text-xs text-ink-faint">
                    <span className="tabular">{formatPlural(playlist.trackCount, 'track')}</span>
                    {' \u00b7 '}
                    {MODE_LABELS[playlist.mode]}
                    {playlist.builtAt ? ` \u00b7 built ${formatRelative(playlist.builtAt)}` : ''}
                  </p>
                </div>
                <Button
                  size="sm"
                  busy={rebuildPlaylist.isPending && rebuildPlaylist.variables === playlist.id}
                  aria-label={`Rebuild ${playlist.name}`}
                  onClick={() => rebuildPlaylist.mutate(playlist.id)}
                >
                  Rebuild
                </Button>
                <Button
                  size="sm"
                  busy={forgetPlaylist.isPending && forgetPlaylist.variables === playlist.id}
                  aria-label={`Stop managing ${playlist.name}`}
                  onClick={() => forgetPlaylist.mutate(playlist.id)}
                >
                  Stop managing
                </Button>
              </li>
            ))}
          </ul>
        )}
        {rebuildPlaylist.isError || forgetPlaylist.isError ? (
          <p role="alert" className="mt-2 text-sm text-ember">
            {errorMessage(rebuildPlaylist.error ?? forgetPlaylist.error)}
          </p>
        ) : null}
      </Panel>

      {/* --- sharing -------------------------------------------------------- */}
      <Panel
        title="Shared links"
        description="Read-only links to your statistics, for people without an account here."
      >
        <p className="max-w-prose text-sm text-ink-muted">
          Totals and rankings only — never individual plays or when they happened. Anyone with the
          link can open it, so treat it as a password.
        </p>

        <form
          className="mt-4 border-t border-seam pt-4"
          onSubmit={(event) => {
            event.preventDefault()
            const body: CreateShareRequest = { label: shareLabel.trim() }
            if (shareWindow !== 'all') body.days = Number(shareWindow)
            createShare.mutate(body)
          }}
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Name" hint="Shown at the top of the shared page.">
              <Input
                value={shareLabel}
                maxLength={80}
                placeholder="My 2026"
                onChange={(event) => setShareLabel(event.target.value)}
              />
            </Field>
            <Field label="Period" hint="A rolling window keeps itself current.">
              <Select value={shareWindow} onChange={(event) => setShareWindow(event.target.value)}>
                <option value="all">Everything</option>
                <option value="7">The last 7 days</option>
                <option value="30">The last 30 days</option>
                <option value="90">The last 90 days</option>
                <option value="365">The last year</option>
              </Select>
            </Field>
          </div>
          <div className="mt-3">
            <Button type="submit" variant="primary" busy={createShare.isPending}>
              Create a link
            </Button>
          </div>
          {createShare.isError ? (
            <p role="alert" className="mt-2 text-sm text-ember">
              {errorMessage(createShare.error)}
            </p>
          ) : null}
        </form>

        {freshLink?.url ? (
          <div className="mt-4 border-t border-seam pt-4">
            <p className="text-sm text-ink">
              Copy this now — Encore stores only a hash of it, so it cannot be shown again.
            </p>
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <Input
                readOnly
                value={freshLink.url}
                onFocus={(event) => event.currentTarget.select()}
                className="min-w-0 flex-1"
              />
              <Button
                onClick={() => {
                  void navigator.clipboard?.writeText(freshLink.url ?? '')
                  toast.notify({ tone: 'success', title: 'Link copied' })
                }}
              >
                Copy
              </Button>
            </div>
          </div>
        ) : null}

        {shares.isPending ? (
          <div className="mt-4 space-y-2" role="status" aria-busy="true" aria-live="polite">
            <span className="sr-only">Loading your links</span>
            <Skeleton className="h-8 w-full" />
          </div>
        ) : (shares.data?.length ?? 0) === 0 ? null : (
          <ul className="mt-4 divide-y divide-seam border-t border-seam">
            {(shares.data ?? []).map((link) => (
              <li key={link.id} className="flex flex-wrap items-center gap-3 py-2.5">
                <div className="min-w-0 flex-1">
                  <p className="text-sm text-ink">{link.label || 'Untitled link'}</p>
                  <p className="text-xs text-ink-faint">
                    <span className="tabular">
                      {link.rolling
                        ? `the last ${formatPlural(link.rangeDays, 'day')}`
                        : 'everything'}
                    </span>
                    {' · '}
                    {link.viewCount === 0
                      ? 'never opened'
                      : `opened ${formatPlural(link.viewCount, 'time')}`}
                    {link.lastViewedAt ? `, last ${formatRelative(link.lastViewedAt)}` : ''}
                  </p>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  busy={revokeShare.isPending && revokeShare.variables === link.id}
                  aria-label={`Revoke ${link.label || 'this link'}`}
                  onClick={() => revokeShare.mutate(link.id)}
                >
                  Revoke
                </Button>
              </li>
            ))}
          </ul>
        )}
        {revokeShare.isError ? (
          <p role="alert" className="mt-2 text-sm text-ember">
            {errorMessage(revokeShare.error)}
          </p>
        ) : null}
      </Panel>

      {/* --- blacklist ------------------------------------------------------ */}
      <Panel
        title="Hidden artists"
        description="Left out of every statistic, every chart and every export."
        padded={false}
      >
        {blacklist.isPending ? (
          <div className="space-y-2 p-4" role="status" aria-busy="true" aria-live="polite">
            <span className="sr-only">Loading hidden artists</span>
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-2/3" />
          </div>
        ) : blacklist.isError ? (
          <ErrorState
            error={blacklist.error}
            title="The hidden artists could not be loaded"
            onRetry={() => {
              void blacklist.refetch()
            }}
          />
        ) : (blacklist.data?.length ?? 0) === 0 ? (
          <EmptyState
            icon="artist"
            title="No artists are hidden"
            description="Open an artist and hide them to leave them out of every statistic. Nothing is deleted
            — showing them again restores every figure."
            action={<ButtonLink to="/artists">Browse your artists</ButtonLink>}
          />
        ) : (
          <ul className="divide-y divide-seam">
            {(blacklist.data ?? []).map((artist) => (
              <li key={artist.id} className="flex items-center gap-3 px-4 py-2.5">
                <div className="min-w-0 flex-1">
                  {artist.name ? (
                    <Link to={`/artists/${artist.id}`} className="text-sm text-ink hover:text-lamp">
                      {artist.name}
                    </Link>
                  ) : (
                    <span className="text-sm text-ink-muted">
                      Not yet named by Spotify
                      <span className="tabular ml-2 text-xs break-all text-ink-faint">
                        {artist.id}
                      </span>
                    </span>
                  )}
                </div>
                <Button
                  size="sm"
                  busy={unhide.isPending && unhide.variables === artist.id}
                  aria-label={`Show ${artist.name || 'this artist'} in statistics again`}
                  onClick={() => unhide.mutate(artist.id)}
                >
                  Show again
                </Button>
              </li>
            ))}
          </ul>
        )}
        {unhide.isError ? (
          <p role="alert" className="px-4 py-3 text-sm text-ember">
            {errorMessage(unhide.error)}
          </p>
        ) : null}
      </Panel>

      {/* --- deletion ------------------------------------------------------- */}
      <Panel title="Delete your account" description="Irreversible. Read this first.">
        <p className="max-w-prose text-sm text-ink-muted">
          Deleting removes your account, every listen, your import jobs, the files you uploaded and
          your Spotify link. There is no undo and no backup — download your data first if you want
          to keep it.
        </p>

        {deleting ? (
          <form
            className="mt-4 border-t border-seam pt-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (confirmMatches) deleteAccount.mutate(confirmName.trim())
            }}
          >
            <Field
              label="Type your Spotify username to confirm"
              hint={
                <>
                  Your username is <span className="tabular">{user.spotifyUserId}</span>.
                </>
              }
            >
              <Input
                value={confirmName}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => setConfirmName(event.target.value)}
              />
            </Field>
            <div className="mt-3 flex flex-wrap gap-2">
              <Button
                type="submit"
                variant="danger"
                busy={deleteAccount.isPending}
                disabled={!confirmMatches}
              >
                Delete my account for good
              </Button>
              <Button
                disabled={deleteAccount.isPending}
                onClick={() => {
                  setDeleting(false)
                  setConfirmName('')
                }}
              >
                Keep my account
              </Button>
            </div>
            {deleteAccount.isError ? (
              <p role="alert" className="mt-2 text-sm text-ember">
                {errorMessage(deleteAccount.error)}
              </p>
            ) : null}
          </form>
        ) : (
          <div className="mt-4">
            <Button variant="danger" onClick={() => setDeleting(true)}>
              Delete my account
            </Button>
          </div>
        )}
      </Panel>

      <p className="text-xs text-ink-faint">
        Account created{' '}
        <span className="tabular">{formatDateTime(user.createdAt, user.timezone)}</span> · last
        signed in{' '}
        <span className="tabular">
          {user.lastLoginAt ? formatDateTime(user.lastLoginAt, user.timezone) : EMPTY}
        </span>
      </p>
    </div>
  )
}

/**
 * What Spotify has told this instance about the music, and what it has not yet.
 *
 * This panel exists because of one specific confusion. A fresh import lands
 * hundreds of thousands of listens in minutes, but the names, artwork and genres
 * behind them arrive one Spotify request at a time, and a development-mode
 * Spotify application has a daily quota it will exhaust in the first hour. The
 * result is a working Encore full of blank artists, with nothing on screen to
 * say whether it is broken, finished, or simply waiting — and the honest answer
 * used to live only in the worker's log.
 *
 * So the panel is written to answer three questions in order: is anything
 * missing, is anything stopping it, and do I need to act. The last answer is
 * almost always no.
 */
function MetadataPanel({
  status,
  error,
  timezone,
}: {
  status: StatusResponse | undefined
  error: unknown
  timezone: string
}): ReactElement {
  const paused = status?.metadata.paused ?? false
  const complete = status?.metadata.complete ?? false

  return (
    <Panel
      title="Music metadata"
      description="Names, artwork and genres, fetched from Spotify after the listens land."
      actions={
        !status ? null : paused ? (
          <Chip tone="warn">Rate limited</Chip>
        ) : complete ? (
          <Chip tone="good">Complete</Chip>
        ) : (
          <Chip tone="info">Filling in</Chip>
        )
      }
    >
      {error ? (
        <p role="alert" className="text-sm text-ember">
          {errorMessage(error)}
        </p>
      ) : !status ? (
        <div className="space-y-3" role="status" aria-busy="true" aria-live="polite">
          <span className="sr-only">Loading metadata status</span>
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-2/3" />
        </div>
      ) : (
        <>
          <div className="space-y-3">
            <ProgressRow label="Tracks" progress={status.catalogue.tracks} />
            <ProgressRow label="Artists" progress={status.catalogue.artists} />
            <ProgressRow label="Albums" progress={status.catalogue.albums} />
          </div>

          {paused && status.metadata.pausedUntil ? (
            <div className="mt-4 border-t border-seam pt-4">
              <p className="max-w-prose text-sm text-ink">
                Spotify has asked Encore to stop calling it until{' '}
                <span className="tabular">
                  {formatDateTime(status.metadata.pausedUntil, timezone)}
                </span>{' '}
                ({formatRelative(status.metadata.pausedUntil)}).{' '}
                {status.metadata.fallbackConfigured
                  ? 'Encore is reading from the fallback source meanwhile, so names and artwork keep arriving.'
                  : 'Fetching resumes by itself then — there is nothing to do and nothing to restart.'}
              </p>
              <p className="mt-2 max-w-prose text-sm text-ink-muted">
                Your listening data is unaffected — only names and artwork wait. Restarting does not
                help; the quota does not reset when the process does.
              </p>
            </div>
          ) : complete ? (
            <p className="mt-4 border-t border-seam pt-4 text-sm text-ink-muted">
              Everything Spotify describes has been fetched. Anything still unnamed is music it no
              longer carries.
            </p>
          ) : (
            <p className="mt-4 border-t border-seam pt-4 text-sm text-ink-muted">
              {formatPlural(status.metadata.outstanding, 'record')} still to fetch, slowly enough to
              stay inside Spotify's rate limit. Statistics do not wait for it.
            </p>
          )}
        </>
      )}
    </Panel>
  )
}

/**
 * One kind of catalogue entity, measured by how much of it is readable.
 *
 * The bar tracks names rather than fully resolved records on purpose: a name is
 * what turns a row of identifiers into a page someone can read, and an import
 * supplies most of them without Spotify's help. Measuring full resolution would
 * show a near-empty bar above a screen that is almost entirely legible.
 */
function ProgressRow({
  label,
  progress,
}: {
  label: string
  progress: EntityProgress
}): ReactElement {
  const share = progress.total > 0 ? Math.min(Math.max(progress.named / progress.total, 0), 1) : 1
  const waiting = progress.pending
  const lost = progress.failed + progress.unavailable

  return (
    <div>
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-sm text-ink">{label}</span>
        <span className="tabular text-xs text-ink-muted">
          {formatCount(progress.named)} of {formatCount(progress.total)} named ·{' '}
          {formatPercent(share, 0)}
        </span>
      </div>
      <div
        className="meter mt-1"
        role="progressbar"
        aria-label={`${label} named`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(share * 100)}
        aria-valuetext={`${formatCount(progress.named)} of ${formatCount(progress.total)} ${label.toLowerCase()} named`}
      >
        <span style={{ width: `${share * 100}%` }} />
      </div>
      <p className="mt-1 text-xs text-ink-faint">
        {waiting > 0
          ? `${formatCount(waiting)} waiting on Spotify`
          : progress.total === 0
            ? 'Nothing imported yet'
            : 'Nothing queued'}
        {progress.local > 0
          ? ` · ${formatCount(progress.local)} named by your import, no artwork yet`
          : ''}
        {lost > 0 ? ` · ${formatCount(lost)} Spotify could not describe` : ''}
      </p>
    </div>
  )
}

function Detail({ label, value }: { label: string; value: ReactNode }): ReactElement {
  return (
    <div className="min-w-0">
      <dt className="eyebrow">{label}</dt>
      <dd className="tabular mt-1 text-sm break-words text-ink">{value}</dd>
    </div>
  )
}
