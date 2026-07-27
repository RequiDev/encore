/**
 * Administration: the two things a self-hosted instance actually needs.
 *
 * Who may sign in, and what the people who have signed in are allowed to do.
 * Every change to an account is confirmed with a sentence about its real
 * consequence, because "delete" on a row of a table is otherwise indistinguish-
 * able from "dismiss".
 *
 * The API refuses to demote, deactivate or delete the last active administrator,
 * which is a rule worth surfacing rather than swallowing: the refusal is shown
 * where the action was attempted, with what to do instead.
 */

import type { ReactElement } from 'react'
import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useEscapeKey } from '../lib/hooks'
import { useSession, useTimeZone } from '../lib/session'
import { EMPTY, formatCount, formatDateTime, formatPlural, formatRelative } from '../lib/format'
import type { AdminSettings, AdminUser, Page, Role, SyncState, User } from '../lib/types'
import {
  Button,
  ButtonLink,
  Checkbox,
  Chip,
  EmptyState,
  ErrorState,
  Ledger,
  LedgerBody,
  LedgerCell,
  LedgerHead,
  LedgerHeaderCell,
  LedgerRow,
  LedgerRowHeader,
  PageHeader,
  Pagination,
  Panel,
  Skeleton,
  SkeletonLedger,
  errorMessage,
  useToast,
} from '../components/ui'
import type { ChipTone } from '../components/ui'

/** Twenty-five rows is a page on a self-hosted instance with a handful of people. */
const PAGE_LIMIT = 25

const SYNC_LABEL: Record<SyncState, string> = {
  ok: 'Syncing',
  needs_reauth: 'Reconnect needed',
  error: 'Sync error',
}

const SYNC_TONE: Record<SyncState, ChipTone> = {
  ok: 'good',
  needs_reauth: 'bad',
  error: 'warn',
}

type ActionKind = 'promote' | 'demote' | 'activate' | 'deactivate' | 'delete'

interface PendingAction {
  user: AdminUser
  kind: ActionKind
}

/** What the confirmation says, in the terms of what will actually happen. */
function actionCopy(pending: PendingAction): { title: string; detail: string; confirm: string } {
  const name = pending.user.displayName || pending.user.spotifyUserId
  switch (pending.kind) {
    case 'promote':
      return {
        title: `Make ${name} an administrator?`,
        detail:
          'They will be able to open registrations, change anybody’s role, deactivate accounts and delete them — including yours.',
        confirm: 'Make administrator',
      }
    case 'demote':
      return {
        title: `Remove ${name}’s administrator rights?`,
        detail:
          'They keep their account, their listening history and their imports, and can still sign in. They lose access to this page.',
        confirm: 'Make an ordinary user',
      }
    case 'deactivate':
      return {
        title: `Deactivate ${name}?`,
        detail:
          'They are refused at sign-in from now on. Nothing is deleted: their listening history is kept and reactivating them restores access exactly as it was.',
        confirm: 'Deactivate account',
      }
    case 'activate':
      return {
        title: `Let ${name} sign in again?`,
        detail:
          'Their history was kept while the account was deactivated, so everything comes back as it was.',
        confirm: 'Activate account',
      }
    default:
      return {
        title: `Delete ${name}?`,
        detail:
          'Their account, every listen, their import jobs and the files they uploaded are removed, along with their Spotify link. This cannot be undone.',
        confirm: 'Delete account',
      }
  }
}

export default function Admin(): ReactElement {
  const { user, isAdmin } = useSession()
  const timeZone = useTimeZone()
  const queryClient = useQueryClient()
  const toast = useToast()
  const [params, setParams] = useSearchParams()

  const [pending, setPending] = useState<PendingAction | null>(null)
  const [note, setNote] = useState('')

  const offset = Math.max(0, Number(params.get('offset') ?? '0') || 0)
  const page = useMemo(() => ({ limit: PAGE_LIMIT, offset }), [offset])

  const settings = useQuery({
    queryKey: qk.adminSettings(),
    queryFn: ({ signal }) => api.get<AdminSettings>('/admin/settings', undefined, signal),
    enabled: isAdmin,
  })

  const users = useQuery({
    queryKey: qk.adminUsers(page),
    queryFn: ({ signal }) => api.get<Page<AdminUser>>('/admin/users', { ...page }, signal),
    enabled: isAdmin,
  })

  const saveSettings = useMutation({
    mutationFn: (registrationsEnabled: boolean) =>
      api.patch<AdminSettings>('/admin/settings', { registrationsEnabled }),
    onSuccess: (next) => {
      queryClient.setQueryData(qk.adminSettings(), next)
      // The instance banner in the shell reads registrations from /api/me.
      void queryClient.invalidateQueries({ queryKey: qk.me() })
      setNote(
        next.registrationsEnabled
          ? 'Registrations are open: anyone who signs in with Spotify gets an account.'
          : 'Registrations are closed: unknown Spotify identities are refused at sign-in.',
      )
    },
  })

  const updateUser = useMutation({
    mutationFn: (input: { id: string; body: { role?: Role; isActive?: boolean } }) =>
      api.patch<User>(`/admin/users/${input.id}`, input.body),
    onSuccess: (updated) => {
      setPending(null)
      void queryClient.invalidateQueries({ queryKey: qk.admin() })
      if (updated.id === user?.id) void queryClient.invalidateQueries({ queryKey: qk.me() })
      const name = updated.displayName || updated.spotifyUserId
      setNote(
        `${name} updated: ${updated.role === 'admin' ? 'administrator' : 'user'}, ${updated.isActive ? 'active' : 'deactivated'}.`,
      )
      toast.notify({ tone: 'success', title: `${name} updated` })
    },
  })

  const removeUser = useMutation({
    mutationFn: (id: string) => api.del<void>(`/admin/users/${id}`),
    onSuccess: (_data, id) => {
      const name = pending?.user.displayName || pending?.user.spotifyUserId || 'The account'
      setPending(null)
      void queryClient.invalidateQueries({ queryKey: qk.admin() })
      if (id === user?.id) void queryClient.invalidateQueries({ queryKey: qk.me() })
      setNote(`${name} was deleted.`)
      toast.notify({ tone: 'success', title: `${name} deleted` })
    },
  })

  useEscapeKey(pending !== null, () => setPending(null))

  if (!isAdmin) {
    return (
      <div className="space-y-5">
        <PageHeader title="Administration" description="Registrations and accounts." />
        <Panel padded={false}>
          <EmptyState
            icon="admin"
            title="You do not have access"
            description="This page is for administrators of this instance. If that should include you, ask whoever runs it to give your account the administrator role."
            action={<ButtonLink to="/settings">Back to settings</ButtonLink>}
          />
        </Panel>
      </div>
    )
  }

  const items = users.data?.items ?? []
  const total = users.data?.total ?? 0
  const registrationsEnabled = settings.data?.registrationsEnabled ?? false
  const actionError = updateUser.error ?? removeUser.error
  const actionBusy = updateUser.isPending || removeUser.isPending

  /** Opens a confirmation, clearing any refusal left over from the last one. */
  const ask = (action: PendingAction | null): void => {
    updateUser.reset()
    removeUser.reset()
    setPending(action)
  }

  const run = (action: PendingAction): void => {
    switch (action.kind) {
      case 'promote':
        updateUser.mutate({ id: action.user.id, body: { role: 'admin' } })
        break
      case 'demote':
        updateUser.mutate({ id: action.user.id, body: { role: 'user' } })
        break
      case 'activate':
        updateUser.mutate({ id: action.user.id, body: { isActive: true } })
        break
      case 'deactivate':
        updateUser.mutate({ id: action.user.id, body: { isActive: false } })
        break
      default:
        removeUser.mutate(action.user.id)
    }
  }

  return (
    <div className="space-y-5">
      <PageHeader
        title="Administration"
        description="Who may sign in to this instance, and what they may do."
      />

      <p role="status" aria-live="polite" className="sr-only">
        {note}
      </p>

      <Panel title="Registrations" description="Who is allowed to create an account here.">
        {settings.isPending ? (
          <div role="status" aria-busy="true" aria-live="polite">
            <span className="sr-only">Loading instance settings</span>
            <Skeleton className="h-5 w-64" />
          </div>
        ) : settings.isError ? (
          <ErrorState
            error={settings.error}
            title="The instance settings could not be loaded"
            onRetry={() => {
              void settings.refetch()
            }}
          />
        ) : (
          <>
            <Checkbox
              label="Let new people sign in"
              hint="Open: anyone who signs in with Spotify gets an account. Closed: new identities are
              refused; existing accounts are unaffected."
              checked={registrationsEnabled}
              disabled={saveSettings.isPending}
              onChange={(event) => saveSettings.mutate(event.target.checked)}
            />
            <p className="mt-3 text-sm text-ink-muted">
              {registrationsEnabled
                ? 'Registrations are open.'
                : 'Registrations are closed. Existing accounts still sign in as usual.'}
            </p>
            {saveSettings.isError ? (
              <p role="alert" className="mt-2 text-sm text-ember">
                {errorMessage(saveSettings.error)}
              </p>
            ) : null}
          </>
        )}
      </Panel>

      <Panel
        title="People"
        description={total > 0 ? `${formatPlural(total, 'account')} on this instance.` : undefined}
        padded={false}
      >
        {users.isPending ? (
          <SkeletonLedger rows={5} columns={5} />
        ) : users.isError ? (
          <ErrorState
            error={users.error}
            title="The user list could not be loaded"
            onRetry={() => {
              void users.refetch()
            }}
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon="admin"
            title="Nobody has signed in yet"
            description="Accounts appear here the first time someone signs in with Spotify. Open registrations above if you want them to be able to."
          />
        ) : (
          <>
            <Ledger caption="Accounts on this instance, with role, activity and sync state">
              <LedgerHead>
                <LedgerRow>
                  <LedgerHeaderCell>Person</LedgerHeaderCell>
                  <LedgerHeaderCell>Role</LedgerHeaderCell>
                  <LedgerHeaderCell>Account</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Listens</LedgerHeaderCell>
                  <LedgerHeaderCell>Sync</LedgerHeaderCell>
                  <LedgerHeaderCell>Last sync</LedgerHeaderCell>
                  <LedgerHeaderCell>Change</LedgerHeaderCell>
                </LedgerRow>
              </LedgerHead>
              <LedgerBody>
                {items.map((row) => {
                  const self = row.id === user?.id
                  const open = pending?.user.id === row.id
                  return (
                    <Fragment key={row.id}>
                      <LedgerRow current={self}>
                        <LedgerRowHeader>
                          <span className="block text-sm text-ink">
                            {row.displayName || row.spotifyUserId}
                            {self ? (
                              <span className="ml-2 text-xs text-ink-faint">(you)</span>
                            ) : null}
                          </span>
                          <span className="tabular block text-xs text-ink-faint">
                            {row.spotifyUserId}
                          </span>
                        </LedgerRowHeader>
                        <LedgerCell>
                          {row.role === 'admin' ? (
                            <Chip tone="lamp">Administrator</Chip>
                          ) : (
                            <Chip>User</Chip>
                          )}
                        </LedgerCell>
                        <LedgerCell>
                          {row.isActive ? (
                            <Chip tone="good">Active</Chip>
                          ) : (
                            <Chip tone="bad">Deactivated</Chip>
                          )}
                        </LedgerCell>
                        <LedgerCell numeric>{formatCount(row.listenCount)}</LedgerCell>
                        <LedgerCell>
                          <Chip tone={SYNC_TONE[row.syncState]}>{SYNC_LABEL[row.syncState]}</Chip>
                        </LedgerCell>
                        <LedgerCell>
                          <span
                            className="tabular text-xs text-ink-muted"
                            title={
                              row.lastSyncAt ? formatDateTime(row.lastSyncAt, timeZone) : undefined
                            }
                          >
                            {row.lastSyncAt ? formatRelative(row.lastSyncAt) : EMPTY}
                          </span>
                        </LedgerCell>
                        <LedgerCell>
                          <div className="flex flex-wrap gap-1.5">
                            <Button
                              size="sm"
                              disabled={actionBusy}
                              onClick={() =>
                                ask({
                                  user: row,
                                  kind: row.role === 'admin' ? 'demote' : 'promote',
                                })
                              }
                            >
                              {row.role === 'admin' ? 'Make a user' : 'Make administrator'}
                            </Button>
                            <Button
                              size="sm"
                              disabled={actionBusy}
                              onClick={() =>
                                ask({
                                  user: row,
                                  kind: row.isActive ? 'deactivate' : 'activate',
                                })
                              }
                            >
                              {row.isActive ? 'Deactivate' : 'Activate'}
                            </Button>
                            <Button
                              size="sm"
                              variant="danger"
                              disabled={actionBusy}
                              onClick={() => ask({ user: row, kind: 'delete' })}
                            >
                              Delete
                            </Button>
                          </div>
                        </LedgerCell>
                      </LedgerRow>
                      {open && pending ? (
                        <LedgerRow>
                          <LedgerCell colSpan={7}>
                            <Confirm
                              pending={pending}
                              busy={actionBusy}
                              error={actionError}
                              onConfirm={() => run(pending)}
                              onCancel={() => ask(null)}
                            />
                          </LedgerCell>
                        </LedgerRow>
                      ) : null}
                    </Fragment>
                  )
                })}
              </LedgerBody>
            </Ledger>
            <Pagination
              label="Accounts"
              total={total}
              limit={PAGE_LIMIT}
              offset={offset}
              onChange={(next) => {
                ask(null)
                setParams(
                  (current) => {
                    const updated = new URLSearchParams(current)
                    if (next <= 0) updated.delete('offset')
                    else updated.set('offset', String(next))
                    return updated
                  },
                  { replace: true },
                )
              }}
            />
          </>
        )}
      </Panel>

      <p className="text-xs text-ink-faint">
        Accounts are created by signing in with Spotify; Encore has no invitations and no passwords
        of its own. The last active administrator cannot be demoted, deactivated or deleted.
      </p>
    </div>
  )
}

/**
 * The confirmation for one account change.
 *
 * It opens inside the table, under the row it is about, so the person it
 * concerns is still on screen. Focus moves to the confirming button and Escape
 * closes it, and a refusal from the server is shown here rather than as a toast
 * that would appear far from the decision.
 */
function Confirm({
  pending,
  busy,
  error,
  onConfirm,
  onCancel,
}: {
  pending: PendingAction
  busy: boolean
  error: unknown
  onConfirm: () => void
  onCancel: () => void
}): ReactElement {
  const box = useRef<HTMLDivElement>(null)
  const copy = actionCopy(pending)

  useEffect(() => {
    box.current?.querySelector('button')?.focus()
  }, [pending])

  const lastAdmin = error instanceof ApiError && error.status === 409

  return (
    <div ref={box} role="group" aria-label={copy.title} className="panel-raised p-4">
      <p className="text-sm font-medium text-ink">{copy.title}</p>
      <p className="mt-1.5 max-w-prose text-sm text-ink-muted">{copy.detail}</p>
      <div className="mt-3 flex flex-wrap gap-2">
        <Button
          variant={pending.kind === 'delete' ? 'danger' : 'primary'}
          busy={busy}
          onClick={onConfirm}
        >
          {copy.confirm}
        </Button>
        <Button disabled={busy} onClick={onCancel}>
          Leave it alone
        </Button>
      </div>
      {error ? (
        <div role="alert" className="mt-3">
          <p className="text-sm text-ember">{errorMessage(error)}</p>
          {lastAdmin ? (
            <p className="mt-1 text-sm text-ink-muted">
              Encore always keeps one active administrator, so that somebody can still manage the
              instance. Promote another account first, then try this again.
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}
