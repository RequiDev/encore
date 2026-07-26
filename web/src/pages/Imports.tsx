/**
 * Imports: where a Spotify export becomes listening history.
 *
 * This page is used under stress — someone has waited weeks for an export and is
 * now watching a four-gigabyte upload crawl — so everything here is literal. The
 * progress bar is driven by `api.upload`, which is XMLHttpRequest rather than
 * fetch for the single reason that fetch cannot report upload progress; a bar
 * animated on a timer would be a lie told to the one person least able to
 * tolerate it.
 *
 * Server warnings are advice, not failures. `already_imported`,
 * `no_history_found` and `empty_archive` all arrive alongside a job that was
 * created and will run, so they are presented as things worth knowing rather
 * than as errors that stopped something.
 */

import type { ChangeEvent, DragEvent, ReactElement } from 'react'
import { useCallback, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useTimeZone } from '../lib/session'
import {
  formatBytes,
  formatCount,
  formatDateTime,
  formatPercent,
  formatPlural,
  formatRelative,
} from '../lib/format'
import type {
  CreateImportResponse,
  ImportJob,
  ImportStatus,
  ImportWarning,
  Page,
} from '../lib/types'
import {
  Button,
  Chip,
  EmptyState,
  ErrorState,
  Field,
  Icon,
  IconButton,
  Input,
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
  SkeletonLedger,
  useToast,
} from '../components/ui'
import type { ChipTone } from '../components/ui'

/** Ten jobs is a screen's worth without a scroll on a laptop. */
const PAGE_LIMIT = 10

/** How often a job that is still moving is re-read for the list. */
const POLL_MS = 4000

const ACCEPT = '.zip,.json,.gz,.json.gz'

const STATUS_LABEL: Record<ImportStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  paused: 'Paused',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
}

const STATUS_TONE: Record<ImportStatus, ChipTone> = {
  queued: 'neutral',
  running: 'lamp',
  paused: 'warn',
  completed: 'good',
  failed: 'bad',
  cancelled: 'neutral',
}

/** A job in one of these states will change on its own, so the list polls. */
function isActive(status: ImportStatus): boolean {
  return status === 'queued' || status === 'running'
}

/**
 * What Encore can read. The extension is checked before the upload rather than
 * after it: telling someone their .txt was not history once they have waited out
 * a gigabyte of transfer would be a poor trade.
 */
function isAccepted(name: string): boolean {
  const lower = name.toLowerCase()
  return lower.endsWith('.zip') || lower.endsWith('.json') || lower.endsWith('.gz')
}

/** Distinguishes staged files without relying on a browser-specific file id. */
function fileKey(file: File): string {
  return `${file.name}:${file.size}:${file.lastModified}`
}

/** Headline for a server warning; the server's own message carries the detail. */
function warningTitle(code: string): string {
  switch (code) {
    case 'already_imported':
      return 'Imported before'
    case 'no_history_found':
      return 'No listening history in this file'
    case 'empty_archive':
      return 'The archive was empty'
    default:
      return 'Worth knowing'
  }
}

/** What to do about it, in one sentence. */
function warningAdvice(code: string): string {
  switch (code) {
    case 'already_imported':
      return 'Nothing will be duplicated — Encore recognises records it already holds — so this file adds nothing and needs no action.'
    case 'no_history_found':
      return 'Spotify’s account-data zip holds several files and only the streaming-history ones carry plays; the rest of the upload imports normally.'
    case 'empty_archive':
      return 'Check you uploaded the zip Spotify sent rather than an empty folder, then upload it again.'
    default:
      return 'The job was still created and will run.'
  }
}

interface UploadProgress {
  loaded: number
  total: number
}

export default function Imports(): ReactElement {
  const timeZone = useTimeZone()
  const queryClient = useQueryClient()
  const toast = useToast()
  const [params, setParams] = useSearchParams()

  const picker = useRef<HTMLInputElement>(null)
  const abort = useRef<AbortController | null>(null)

  const [staged, setStaged] = useState<File[]>([])
  const [note, setNote] = useState('')
  const [dragging, setDragging] = useState(false)
  const [ignored, setIgnored] = useState<string[]>([])
  const [progress, setProgress] = useState<UploadProgress | null>(null)
  const [status, setStatus] = useState('')
  const [accepted, setAccepted] = useState<CreateImportResponse | null>(null)

  const offset = Math.max(0, Number(params.get('offset') ?? '0') || 0)
  const page = useMemo(() => ({ limit: PAGE_LIMIT, offset }), [offset])

  const jobs = useQuery({
    queryKey: qk.importList(page),
    queryFn: ({ signal }) => api.get<Page<ImportJob>>('/imports', { ...page }, signal),
    // A queued or running job changes without anybody touching the page, so the
    // list keeps itself current while one is in flight and stops as soon as
    // none is — polling a page of finished jobs would be pure server load.
    refetchInterval: (query) =>
      query.state.data?.items.some((job) => isActive(job.status)) ? POLL_MS : false,
  })

  const addFiles = useCallback((incoming: FileList | null) => {
    if (!incoming || incoming.length === 0) return
    const chosen = Array.from(incoming)
    const good = chosen.filter((file) => isAccepted(file.name))
    const bad = chosen.filter((file) => !isAccepted(file.name)).map((file) => file.name)

    setIgnored(bad)
    setStaged((current) => {
      const seen = new Set(current.map(fileKey))
      return [...current, ...good.filter((file) => !seen.has(fileKey(file)))]
    })
    if (good.length > 0) setStatus(`${formatPlural(good.length, 'file')} ready to upload.`)
  }, [])

  const upload = useMutation({
    mutationFn: async (payload: { files: File[]; note: string }) => {
      const form = new FormData()
      for (const file of payload.files) form.append('files', file, file.name)
      const trimmed = payload.note.trim()
      if (trimmed) form.append('note', trimmed)

      const controller = new AbortController()
      abort.current = controller
      return api.upload<CreateImportResponse>('/imports', form, {
        signal: controller.signal,
        onUploadProgress: (loaded, total) => setProgress({ loaded, total }),
      })
    },
    onSuccess: (response) => {
      setAccepted(response)
      setStaged([])
      setNote('')
      setIgnored([])
      setProgress(null)
      setStatus(
        `Upload finished. ${formatPlural(response.job.filesTotal, 'file')} queued for import.`,
      )
      void queryClient.invalidateQueries({ queryKey: qk.imports() })
      toast.notify({
        tone: 'success',
        title: 'Import queued',
        description: `${formatPlural(response.job.filesTotal, 'file')} will be read in the background.`,
      })
    },
    onError: (error) => {
      setProgress(null)
      if (error instanceof ApiError && error.code === 'aborted') {
        setStatus('Upload cancelled. Nothing was imported.')
        toast.notify({ title: 'Upload cancelled', description: 'Nothing was imported.' })
        return
      }
      setStatus('The upload did not finish.')
    },
    onSettled: () => {
      abort.current = null
    },
  })

  const totalBytes = staged.reduce((sum, file) => sum + file.size, 0)
  const percent = progress && progress.total > 0 ? progress.loaded / progress.total : 0
  const busy = upload.isPending
  const cancelled = upload.error instanceof ApiError && upload.error.code === 'aborted'

  const items = jobs.data?.items ?? []
  const total = jobs.data?.total ?? 0

  return (
    <div className="space-y-5">
      <PageHeader
        title="Imports"
        description="Upload a Spotify export and watch it land."
        actions={
          <a
            className="btn"
            href="https://www.spotify.com/account/privacy/"
            target="_blank"
            rel="noreferrer noopener"
          >
            Request your data
            <Icon name="external" />
            <span className="sr-only">(opens in a new tab)</span>
          </a>
        }
      />

      <p role="status" aria-live="polite" className="sr-only">
        {status}
      </p>

      <div className="grid gap-5 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <Panel title="Upload an export" description="Zip, JSON or gzipped JSON — several at once.">
          <label
            className={[
              'flex flex-col items-center gap-2 rounded-panel border border-dashed px-4 py-8 text-center',
              'focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-lamp',
              dragging ? 'border-lamp text-ink' : 'border-seam-strong text-ink-muted',
              busy ? 'pointer-events-none opacity-60' : 'cursor-pointer hover:border-lamp-dim',
            ].join(' ')}
            onDragEnter={(event: DragEvent<HTMLLabelElement>) => {
              event.preventDefault()
              setDragging(true)
            }}
            onDragOver={(event: DragEvent<HTMLLabelElement>) => {
              // Without this the browser navigates to the dropped file instead.
              event.preventDefault()
              setDragging(true)
            }}
            onDragLeave={(event: DragEvent<HTMLLabelElement>) => {
              const next = event.relatedTarget
              // Moving between children fires dragleave on the parent; only a
              // pointer that has genuinely left the zone should unlight it.
              if (next instanceof Node && event.currentTarget.contains(next)) return
              setDragging(false)
            }}
            onDrop={(event: DragEvent<HTMLLabelElement>) => {
              event.preventDefault()
              setDragging(false)
              addFiles(event.dataTransfer.files)
            }}
          >
            <input
              ref={picker}
              type="file"
              multiple
              accept={ACCEPT}
              disabled={busy}
              className="sr-only"
              onChange={(event: ChangeEvent<HTMLInputElement>) => {
                addFiles(event.target.files)
                // Cleared so choosing the same file twice still fires a change.
                event.target.value = ''
              }}
            />
            <Icon name="import" size={24} />
            <span className="text-sm">
              Drop your export here, or <span className="font-medium text-lamp">choose files</span>
            </span>
            <span className="text-xs text-ink-faint">
              The zip Spotify sends is the easy path: Encore opens it and finds the history inside.
            </span>
          </label>

          {ignored.length > 0 ? (
            <p className="mt-3 text-xs text-ink-muted">
              Left out, because Encore reads only .zip, .json and .json.gz:{' '}
              <span className="tabular break-all">{ignored.join(', ')}</span>
            </p>
          ) : null}

          {staged.length > 0 ? (
            <>
              <ul className="mt-4 divide-y divide-seam border-y border-seam">
                {staged.map((file) => (
                  <li key={fileKey(file)} className="flex items-center gap-3 py-2">
                    <span className="min-w-0 flex-1 truncate text-sm text-ink" title={file.name}>
                      {file.name}
                    </span>
                    <span className="tabular shrink-0 text-xs text-ink-muted">
                      {formatBytes(file.size)}
                    </span>
                    <IconButton
                      label={`Remove ${file.name}`}
                      disabled={busy}
                      onClick={() =>
                        setStaged((current) => current.filter((f) => fileKey(f) !== fileKey(file)))
                      }
                    >
                      <Icon name="close" size={14} />
                    </IconButton>
                  </li>
                ))}
              </ul>

              <div className="mt-4 space-y-4">
                <Field
                  label="Note"
                  hint="Optional. A word about what this is, so the list below still makes sense in a year."
                >
                  <Input
                    value={note}
                    maxLength={200}
                    disabled={busy}
                    placeholder="full export, June 2026"
                    onChange={(event) => setNote(event.target.value)}
                  />
                </Field>

                <div className="flex flex-wrap items-center gap-3">
                  <Button
                    variant="primary"
                    busy={busy}
                    onClick={() => {
                      setAccepted(null)
                      setStatus(
                        `Uploading ${formatPlural(staged.length, 'file')}, ${formatBytes(totalBytes)}.`,
                      )
                      upload.mutate({ files: staged, note })
                    }}
                  >
                    Import {formatPlural(staged.length, 'file')}
                  </Button>
                  <span className="tabular text-xs text-ink-muted">{formatBytes(totalBytes)}</span>
                  {busy ? (
                    <Button
                      onClick={() => {
                        abort.current?.abort()
                      }}
                    >
                      Cancel upload
                    </Button>
                  ) : null}
                </div>
              </div>
            </>
          ) : null}

          {progress ? (
            <div className="mt-4">
              <div className="flex items-baseline justify-between gap-3">
                <p className="eyebrow">Uploading</p>
                <p className="tabular text-xs text-ink-muted">
                  {formatBytes(progress.loaded)} of {formatBytes(progress.total)} ·{' '}
                  {formatPercent(percent, 0)}
                </p>
              </div>
              <div
                className="meter mt-2 h-1.5"
                role="progressbar"
                aria-label="Upload progress"
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={Math.round(percent * 100)}
                aria-valuetext={`${formatPercent(percent, 0)} uploaded`}
              >
                <span style={{ width: `${percent * 100}%` }} />
              </div>
              <p className="mt-2 text-xs text-ink-faint">
                Keep this tab open until the upload finishes. The import itself runs on the server
                and carries on without you.
              </p>
            </div>
          ) : null}

          {upload.isError && !cancelled ? (
            <div className="mt-4">
              <ErrorState
                error={upload.error}
                title="The upload did not finish"
                onRetry={
                  staged.length > 0
                    ? () => {
                        upload.mutate({ files: staged, note })
                      }
                    : undefined
                }
              />
            </div>
          ) : null}

          {accepted ? (
            <div className="mt-4 space-y-3 border-t border-seam pt-4">
              <p className="text-sm text-ink">
                Queued. {formatPlural(accepted.job.filesTotal, 'file')} will be read in the
                background — you can leave this page.
              </p>
              <Link className="btn btn-primary" to={`/imports/${accepted.job.id}`}>
                Watch this job
                <Icon name="chevron-right" />
              </Link>
              {accepted.warnings.length > 0 ? (
                <ul className="space-y-3 pt-1">
                  {accepted.warnings.map((warning, index) => (
                    <WarningRow
                      key={`${warning.file}-${warning.code}-${index}`}
                      warning={warning}
                    />
                  ))}
                </ul>
              ) : null}
            </div>
          ) : null}
        </Panel>

        <Panel title="Where the files come from">
          <div className="space-y-3 text-sm text-ink-muted">
            <p>
              Spotify offers two exports.{' '}
              <strong className="font-semibold text-ink">Account data</strong> arrives in a few days
              and covers roughly the last year;{' '}
              <strong className="font-semibold text-ink">extended streaming history</strong> takes a
              few weeks and covers everything you have ever played. Ask for either on your Spotify
              privacy page, then upload the zip exactly as it arrives — Encore opens it, takes the
              streaming-history files and ignores the rest.
            </p>
            <p>
              Re-importing is safe: Encore recognises records it already holds, so uploading the
              same export twice adds nothing and changes no statistic.
            </p>
            <p>
              <a
                className="text-lamp underline underline-offset-2"
                href="https://www.spotify.com/account/privacy/"
                target="_blank"
                rel="noreferrer noopener"
              >
                spotify.com/account/privacy
                <span className="sr-only"> (opens in a new tab)</span>
              </a>
            </p>
          </div>
        </Panel>
      </div>

      <Panel
        title="Past imports"
        description={total > 0 ? `${formatPlural(total, 'job')}, newest first.` : undefined}
        padded={false}
        actions={
          jobs.isFetching && !jobs.isPending ? (
            <span className="eyebrow" aria-hidden="true">
              Refreshing
            </span>
          ) : null
        }
      >
        {jobs.isPending ? (
          <SkeletonLedger rows={4} columns={5} />
        ) : jobs.isError ? (
          <ErrorState
            error={jobs.error}
            title="Your imports could not be loaded"
            onRetry={() => {
              void jobs.refetch()
            }}
          />
        ) : items.length === 0 ? (
          <EmptyState
            icon="import"
            title="No imports yet"
            description="Once you upload an export it appears here with its counters, and stays as the record of what arrived when."
            action={
              <Button
                variant="primary"
                onClick={() => {
                  picker.current?.click()
                }}
              >
                Choose files
              </Button>
            }
          />
        ) : (
          <>
            <Ledger caption="Import jobs, newest first">
              <LedgerHead>
                <LedgerRow>
                  <LedgerHeaderCell>Started</LedgerHeaderCell>
                  <LedgerHeaderCell>Status</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Files</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Imported</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Duplicates</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Skipped</LedgerHeaderCell>
                  <LedgerHeaderCell numeric>Rejected</LedgerHeaderCell>
                </LedgerRow>
              </LedgerHead>
              <LedgerBody>
                {items.map((job) => (
                  <LedgerRow key={job.id}>
                    <LedgerRowHeader>
                      <Link
                        to={`/imports/${job.id}`}
                        className="block min-w-0 hover:text-lamp"
                        title={job.note || undefined}
                      >
                        <span className="tabular block text-sm">
                          {formatDateTime(job.createdAt, timeZone)}
                        </span>
                        <span className="block truncate text-xs text-ink-faint">
                          {job.note || formatRelative(job.createdAt)}
                        </span>
                      </Link>
                    </LedgerRowHeader>
                    <LedgerCell>
                      <Chip tone={STATUS_TONE[job.status]}>{STATUS_LABEL[job.status]}</Chip>
                    </LedgerCell>
                    <LedgerCell numeric>
                      {formatCount(job.filesDone)} / {formatCount(job.filesTotal)}
                    </LedgerCell>
                    <LedgerCell numeric>{formatCount(job.counters.imported)}</LedgerCell>
                    <LedgerCell numeric>{formatCount(job.counters.duplicates)}</LedgerCell>
                    <LedgerCell numeric>{formatCount(job.counters.skipped)}</LedgerCell>
                    <LedgerCell numeric>{formatCount(job.counters.rejected)}</LedgerCell>
                  </LedgerRow>
                ))}
              </LedgerBody>
            </Ledger>
            <Pagination
              label="Imports"
              total={total}
              limit={PAGE_LIMIT}
              offset={offset}
              onChange={(next) => {
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
    </div>
  )
}

/** One piece of advice about an upload that was accepted anyway. */
function WarningRow({ warning }: { warning: ImportWarning }): ReactElement {
  return (
    <li className="border-l-2 border-lamp-dim pl-3">
      <p className="text-sm font-medium text-ink">{warningTitle(warning.code)}</p>
      {warning.file ? (
        <p className="tabular mt-0.5 text-xs break-all text-ink-faint">{warning.file}</p>
      ) : null}
      {warning.message ? <p className="mt-1 text-sm text-ink-muted">{warning.message}</p> : null}
      <p className="mt-1 text-sm text-ink-muted">{warningAdvice(warning.code)}</p>
    </li>
  )
}
