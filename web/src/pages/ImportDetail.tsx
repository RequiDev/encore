/**
 * One import job, watched while it runs.
 *
 * Two rules shape this page. A denominator is never invented: `recordsTotal` is
 * null until a file has been read to the end, and until then the file says how
 * many records it has read and that it is still counting, rather than showing a
 * percentage of a number nobody knows. And the job is polled only while it can
 * still change — a finished job is a static document, and polling one would cost
 * the server a query every few seconds for an answer that will never move.
 *
 * The three controls all destroy or restart something, so each states what it
 * will actually do before it does it: cancelling keeps what has been imported,
 * retrying resumes from the checkpoint rather than starting again, and deleting
 * removes the job record and the upload while leaving every listen in place.
 */

import type { ReactElement, ReactNode } from 'react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from '../lib/api'
import { qk } from '../lib/query'
import { useEscapeKey } from '../lib/hooks'
import { useTimeZone } from '../lib/session'
import {
  EMPTY,
  formatBytes,
  formatCount,
  formatDateTime,
  formatPercent,
  formatPlural,
} from '../lib/format'
import type {
  ImportFile,
  ImportFileStatus,
  ImportFormat,
  ImportJob,
  ImportReject,
  ImportStatus,
  Page,
} from '../lib/types'
import {
  Button,
  ButtonLink,
  Chip,
  EmptyState,
  ErrorState,
  Icon,
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
  Stat,
  StatGrid,
  errorMessage,
  useToast,
} from '../components/ui'
import type { ChipTone } from '../components/ui'

/** Fast enough to feel live, slow enough to be free on a job that runs for hours. */
const POLL_MS = 3000

/** Rejected records per page. Each one is several lines, so a page is short. */
const REJECT_LIMIT = 20

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

const FILE_STATUS_LABEL: Record<ImportFileStatus, string> = {
  pending: 'Waiting',
  running: 'Reading',
  completed: 'Done',
  failed: 'Failed',
  skipped: 'Skipped',
}

const FILE_STATUS_TONE: Record<ImportFileStatus, ChipTone> = {
  pending: 'neutral',
  running: 'lamp',
  completed: 'good',
  failed: 'bad',
  skipped: 'neutral',
}

const FORMAT_LABEL: Record<ImportFormat, string> = {
  extended: 'Extended history',
  account_data: 'Account data',
  unknown: 'Unrecognised',
}

/** True while the job can still change on its own. */
function isActive(status: ImportStatus): boolean {
  return status === 'queued' || status === 'running'
}

/** What a failure means, and what to do about it, in one sentence each. */
function failureAdvice(code: string): string {
  switch (code) {
    case 'verification_failed':
      return 'Encore counted the records it wrote and found fewer of them in the database than it had reported importing, so it refused to call the job finished. Resume it — the count runs again — and if it fails a second time the server log names the file.'
    case 'file_unreadable':
      return 'A file could not be read to the end. The upload may have been cut short, or the archive may be damaged; download the export from Spotify again and upload it fresh.'
    case 'retries_exhausted':
      return 'The importer kept meeting a failure it expected to be temporary — usually the database being briefly unavailable — and stopped after several attempts. Check the server is healthy, then resume; it carries on from its checkpoint.'
    case 'unrecognised_format':
      return 'This is not a file Encore can read. Upload the zip Spotify sent, or the Streaming_History JSON files from inside it.'
    case 'empty_upload':
      return 'The upload held no records at all. Check it is the file Spotify sent and that the archive is not empty, then upload it again.'
    case 'internal_error':
      return 'Something went wrong inside Encore rather than in your file. Resume the job; if it fails again the server log holds the detail this page deliberately does not.'
    default:
      return 'Resume the job to try again from where it stopped. If it fails the same way twice, the server log holds the detail.'
  }
}

/** Why a single record could never be imported. */
function rejectAdvice(reason: string): string {
  switch (reason) {
    case 'malformed_record':
      return 'The record was not valid JSON where a record was expected.'
    case 'missing_timestamp':
      return 'The record carried no play time, so there is nothing to place it on a timeline.'
    case 'invalid_timestamp':
      return 'The play time could not be read as a date.'
    case 'timestamp_out_of_range':
      return 'The play time falls outside the years Spotify has existed, so it cannot describe a real listen.'
    case 'missing_track_identity':
      return 'The record named neither a track nor an artist, so the play cannot be attributed to anything.'
    case 'invalid_ms_played':
      return 'The play length was missing or was not a number.'
    case 'unrecognised_record_shape':
      return 'The record matches neither export format Encore reads.'
    default:
      return 'Encore could not make a listen out of this record.'
  }
}

/**
 * The server names the file each rejected record came from. `lib/types.ts` does
 * not carry that field, so it is read optionally here rather than by editing the
 * shared contract: present it when it is there, ignore it when it is not.
 */
type RejectRow = ImportReject & { file?: string }

/**
 * What the live region says about a job.
 *
 * Derived rather than remembered, which is what keeps it from announcing on
 * every poll: the sentence for a running job deliberately carries no counters,
 * so its text is identical from one refresh to the next and a screen reader
 * hears it once, when the status actually changes. The terminal sentences may
 * quote figures freely, because a finished job's figures are final.
 */
function jobSentence(job: ImportJob): string {
  switch (job.status) {
    case 'queued':
      return 'Import queued. It starts as soon as a worker is free.'
    case 'running':
      return 'Import running. The figures on this page update every few seconds.'
    case 'paused':
      return 'Import paused. Resume it to carry on from where it stopped.'
    case 'completed':
      return `Import finished: ${formatPlural(job.counters.imported, 'listen')} imported.`
    case 'failed':
      return `Import failed. ${job.errorMessage || failureAdvice(job.errorCode)}`
    case 'cancelled':
      return 'Import cancelled. Everything imported before it stopped was kept.'
    default:
      return ''
  }
}

type PendingAction = 'cancel' | 'retry' | 'delete'

export default function ImportDetail(): ReactElement {
  const { id = '' } = useParams<{ id: string }>()
  const timeZone = useTimeZone()
  const navigate = useNavigate()
  const toast = useToast()
  const queryClient = useQueryClient()
  const [params, setParams] = useSearchParams()

  const [pending, setPending] = useState<PendingAction | null>(null)

  const job = useQuery({
    queryKey: qk.importJob(id),
    queryFn: ({ signal }) => api.get<ImportJob>(`/imports/${id}`, undefined, signal),
    enabled: id !== '',
    // Terminal jobs never change again, so the polling stops rather than
    // idling at a few requests a minute for the life of the tab.
    refetchInterval: (query) =>
      query.state.data && isActive(query.state.data.status) ? POLL_MS : false,
  })

  const data = job.data
  const active = data ? isActive(data.status) : false
  const rejectedCount = data?.counters.rejected ?? 0

  const offset = Math.max(0, Number(params.get('offset') ?? '0') || 0)
  const rejectPage = useMemo(() => ({ limit: REJECT_LIMIT, offset }), [offset])

  const rejects = useQuery({
    queryKey: qk.importRejects(id, rejectPage),
    queryFn: ({ signal }) =>
      api.get<Page<RejectRow>>(`/imports/${id}/rejects`, { ...rejectPage }, signal),
    enabled: id !== '' && rejectedCount > 0,
    refetchInterval: active ? POLL_MS * 3 : false,
  })

  // A status that changes while somebody is reading the files table is silent
  // otherwise. The toast provider's own live region carries the confirmations
  // of the three controls, so this one only has to carry the job's own state.
  const announcement = data ? jobSentence(data) : ''

  const afterChange = (next: ImportJob, message: string): void => {
    queryClient.setQueryData(qk.importJob(id), next)
    // `qk.imports()` is the prefix of every import key, so this refreshes the
    // list on the page behind as well as this job.
    void queryClient.invalidateQueries({ queryKey: qk.imports() })
    setPending(null)
    toast.notify({ title: message })
  }

  const cancel = useMutation({
    mutationFn: () => api.post<ImportJob>(`/imports/${id}/cancel`),
    onSuccess: (next) => afterChange(next, 'Import stopping. Everything already imported is kept.'),
  })

  const retry = useMutation({
    mutationFn: () => api.post<ImportJob>(`/imports/${id}/retry`),
    onSuccess: (next) => afterChange(next, 'Import resumed from its checkpoint.'),
  })

  const remove = useMutation({
    mutationFn: () => api.del<void>(`/imports/${id}`),
    onSuccess: () => {
      setPending(null)
      queryClient.removeQueries({ queryKey: qk.importJob(id) })
      void queryClient.invalidateQueries({ queryKey: qk.imports() })
      toast.notify({
        tone: 'success',
        title: 'Import job deleted',
        description: 'Your listening records were kept.',
      })
      void navigate('/imports', { replace: true })
    },
  })

  const busy = cancel.isPending || retry.isPending || remove.isPending
  const actionError = cancel.error ?? retry.error ?? remove.error

  /** Opens a confirmation, clearing whatever the last attempt left behind. */
  const ask = (next: PendingAction | null): void => {
    cancel.reset()
    retry.reset()
    remove.reset()
    setPending(next)
  }

  useEscapeKey(pending !== null, () => ask(null))

  // --- whole-page states ---------------------------------------------------

  if (job.isPending) {
    return (
      <Shell>
        <p role="status" aria-live="polite" className="sr-only">
          Loading this import job.
        </p>
        <Panel>
          <Skeleton className="h-6 w-40" />
          <Skeleton className="mt-3 h-3 w-64" />
        </Panel>
        <Panel padded={false}>
          <SkeletonLedger rows={5} columns={5} />
        </Panel>
      </Shell>
    )
  }

  if (job.isError || !data) {
    // A job that never existed, or that belongs to somebody else, comes back as
    // a 404 — and retrying it would fail identically, so it is not offered.
    const missing = job.error instanceof ApiError && job.error.isNotFound
    return (
      <Shell>
        <Panel padded={false}>
          <ErrorState
            error={job.error}
            title={missing ? 'There is no such import job' : 'This import job could not be loaded'}
            onRetry={
              missing
                ? undefined
                : () => {
                    void job.refetch()
                  }
            }
          >
            <ButtonLink to="/imports">Back to imports</ButtonLink>
          </ErrorState>
        </Panel>
      </Shell>
    )
  }

  const filesShare = data.filesTotal > 0 ? Math.min(data.filesDone / data.filesTotal, 1) : 0
  const canCancel = data.status === 'queued' || data.status === 'running'
  const canRetry =
    data.status === 'failed' || data.status === 'cancelled' || data.status === 'paused'

  return (
    <Shell
      description={data.note || `Started ${formatDateTime(data.createdAt, timeZone)}`}
      status={STATUS_LABEL[data.status]}
      tone={STATUS_TONE[data.status]}
    >
      <p role="status" aria-live="polite" className="sr-only">
        {announcement}
      </p>

      <Panel title="This job" description={`${formatPlural(data.filesTotal, 'file')} in the job.`}>
        <dl className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-3">
          <Detail label="Created" value={formatDateTime(data.createdAt, timeZone)} />
          <Detail
            label="Started"
            value={data.startedAt ? formatDateTime(data.startedAt, timeZone) : 'Not yet'}
          />
          <Detail
            label="Finished"
            value={data.finishedAt ? formatDateTime(data.finishedAt, timeZone) : EMPTY}
          />
        </dl>

        <div className="mt-4">
          <div className="flex items-baseline justify-between gap-3">
            <p className="eyebrow">Files read</p>
            <p className="tabular text-xs text-ink-muted">
              {formatCount(data.filesDone)} / {formatCount(data.filesTotal)}
            </p>
          </div>
          <div
            className="meter mt-2 h-1.5"
            role="progressbar"
            aria-label="Files read"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(filesShare * 100)}
            aria-valuetext={`${formatCount(data.filesDone)} of ${formatCount(data.filesTotal)} files read`}
          >
            <span style={{ width: `${filesShare * 100}%` }} />
          </div>
        </div>

        <div className="mt-5 flex flex-wrap items-center gap-2 border-t border-seam pt-4">
          <Button disabled={!canCancel || busy} onClick={() => ask('cancel')}>
            Stop import
          </Button>
          <Button disabled={!canRetry || busy} onClick={() => ask('retry')}>
            <Icon name="refresh" />
            Resume import
          </Button>
          <Button
            variant="danger"
            disabled={busy || active}
            onClick={() => ask('delete')}
            title={active ? 'Stop the import before deleting the job.' : undefined}
          >
            Delete job
          </Button>
          {active ? (
            <span className="text-xs text-ink-faint">
              A job can only be deleted once it has stopped.
            </span>
          ) : null}
        </div>

        {pending === 'cancel' ? (
          <Confirm
            title="Stop this import?"
            detail="It stops at the next batch boundary. Everything imported so far is kept, and you can
            resume from there."
            confirmLabel="Stop import"
            busy={cancel.isPending}
            onConfirm={() => cancel.mutate()}
            onCancel={() => ask(null)}
          />
        ) : null}

        {pending === 'retry' ? (
          <Confirm
            title="Resume this import?"
            detail="It picks up from its checkpoint: finished files are not read again, and a part-read
            file continues where it stopped."
            confirmLabel="Resume import"
            busy={retry.isPending}
            onConfirm={() => retry.mutate()}
            onCancel={() => ask(null)}
          />
        ) : null}

        {pending === 'delete' ? (
          <Confirm
            title="Delete this job record?"
            detail="The job and the files you uploaded are removed. Your listening records stay. This
            cannot be undone."
            confirmLabel="Delete job"
            destructive
            busy={remove.isPending}
            onConfirm={() => remove.mutate()}
            onCancel={() => ask(null)}
          />
        ) : null}

        {actionError ? (
          <p role="alert" className="mt-3 text-sm text-ember">
            {errorMessage(actionError)}
          </p>
        ) : null}
      </Panel>

      {data.status === 'failed' ? (
        <Panel title="Why it failed">
          <p className="text-sm text-ink">{failureAdvice(data.errorCode)}</p>
          {data.errorMessage ? (
            <p className="mt-2 text-sm text-ink-muted">{data.errorMessage}</p>
          ) : null}
          {data.errorCode ? (
            <p className="tabular mt-2 text-xs text-ink-faint">{data.errorCode}</p>
          ) : null}
        </Panel>
      ) : null}

      <StatGrid columns={4}>
        <Stat label="Imported" value={formatCount(data.counters.imported)} lamp />
        <Stat
          label="Duplicates"
          value={formatCount(data.counters.duplicates)}
          hint="Already in your history"
        />
        <Stat
          label="Skipped"
          value={formatCount(data.counters.skipped)}
          hint="Podcasts, local files and plays too short to count"
        />
        <Stat
          label="Rejected"
          value={formatCount(data.counters.rejected)}
          hint="Records Encore could not read"
        />
      </StatGrid>

      <Panel title="Files" padded={false}>
        {data.files.length === 0 ? (
          <EmptyState
            icon="import"
            title="No files in this job"
            description="Nothing was staged for this job. Upload an export to start a new one."
            action={<ButtonLink to="/imports">Back to imports</ButtonLink>}
          />
        ) : (
          <Ledger caption="Files in this import job, with progress and counters">
            <LedgerHead>
              <LedgerRow>
                <LedgerHeaderCell>File</LedgerHeaderCell>
                <LedgerHeaderCell>Format</LedgerHeaderCell>
                <LedgerHeaderCell>Status</LedgerHeaderCell>
                <LedgerHeaderCell>Records read</LedgerHeaderCell>
                <LedgerHeaderCell numeric>Imported</LedgerHeaderCell>
                <LedgerHeaderCell numeric>Duplicates</LedgerHeaderCell>
                <LedgerHeaderCell numeric>Skipped</LedgerHeaderCell>
                <LedgerHeaderCell numeric>Rejected</LedgerHeaderCell>
              </LedgerRow>
            </LedgerHead>
            <LedgerBody>
              {data.files.map((file) => (
                <FileRows key={file.id} file={file} />
              ))}
            </LedgerBody>
          </Ledger>
        )}
      </Panel>

      <Panel
        title="Rejected records"
        description="Records Encore could not turn into a listen. Everything else in the file still imported."
        padded={false}
      >
        {rejectedCount === 0 ? (
          <EmptyState
            icon="check"
            title="Nothing was rejected"
            description="Every record in this job was either imported, already known, or deliberately skipped."
          />
        ) : rejects.isPending ? (
          <SkeletonLedger rows={4} columns={2} />
        ) : rejects.isError ? (
          <ErrorState
            error={rejects.error}
            title="The rejected records could not be loaded"
            onRetry={() => {
              void rejects.refetch()
            }}
          />
        ) : (rejects.data?.items.length ?? 0) === 0 ? (
          <EmptyState
            title="Nothing on this page"
            description="The rejected records may have moved while the job was running. Go back to the first page."
            action={
              <Button
                onClick={() => {
                  setParams(
                    (current) => {
                      const updated = new URLSearchParams(current)
                      updated.delete('offset')
                      return updated
                    },
                    { replace: true },
                  )
                }}
              >
                First page
              </Button>
            }
          />
        ) : (
          <>
            <ol className="divide-y divide-seam">
              {(rejects.data?.items ?? []).map((reject) => (
                <RejectItem
                  key={`${reject.file ?? ''}-${reject.recordIndex}-${reject.createdAt}`}
                  reject={reject}
                />
              ))}
            </ol>
            <Pagination
              label="Rejected records"
              total={rejects.data?.total ?? 0}
              limit={REJECT_LIMIT}
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
    </Shell>
  )
}

// --- page furniture --------------------------------------------------------

/**
 * The page's one h1, kept outside every branch so a failed load still has a
 * title, a way back and a document title.
 */
function Shell({
  description,
  status,
  tone,
  children,
}: {
  description?: string
  status?: string
  tone?: ChipTone
  children: ReactNode
}): ReactElement {
  return (
    <div className="space-y-5">
      <PageHeader
        title="Import job"
        description={description}
        documentTitle="Import job"
        actions={
          <>
            {status ? <Chip tone={tone}>{status}</Chip> : null}
            <ButtonLink to="/imports">
              <Icon name="chevron-left" />
              All imports
            </ButtonLink>
          </>
        }
      />
      {children}
    </div>
  )
}

function Detail({ label, value }: { label: string; value: string }): ReactElement {
  return (
    <div className="min-w-0">
      <dt className="eyebrow">{label}</dt>
      <dd className="tabular mt-1 text-sm text-ink">{value}</dd>
    </div>
  )
}

/**
 * A confirmation that says what the button will actually do.
 *
 * Focus moves to the confirming control when it opens and Escape closes it, so
 * the decision can be taken and abandoned entirely from the keyboard.
 */
function Confirm({
  title,
  detail,
  confirmLabel,
  destructive = false,
  busy,
  onConfirm,
  onCancel,
}: {
  title: string
  detail: string
  confirmLabel: string
  destructive?: boolean
  busy: boolean
  onConfirm: () => void
  onCancel: () => void
}): ReactElement {
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    box.current?.querySelector('button')?.focus()
  }, [])

  return (
    <div ref={box} role="group" aria-label={title} className="panel-raised mt-4 p-4">
      <p className="text-sm font-medium text-ink">{title}</p>
      <p className="mt-1.5 max-w-prose text-sm text-ink-muted">{detail}</p>
      <div className="mt-3 flex flex-wrap gap-2">
        <Button variant={destructive ? 'danger' : 'primary'} busy={busy} onClick={onConfirm}>
          {confirmLabel}
        </Button>
        <Button disabled={busy} onClick={onCancel}>
          Keep as it is
        </Button>
      </div>
    </div>
  )
}

/** One file: its row, plus a second row for its error when it has one. */
function FileRows({ file }: { file: ImportFile }): ReactElement {
  return (
    <>
      <LedgerRow>
        <LedgerRowHeader>
          <span className="block max-w-72 truncate text-sm text-ink" title={file.name}>
            {file.name}
          </span>
          <span
            className="block max-w-72 truncate text-xs text-ink-faint"
            title={file.containerPath || undefined}
          >
            {file.containerPath || formatBytes(file.sizeBytes)}
          </span>
        </LedgerRowHeader>
        <LedgerCell>
          <Chip tone={file.format === 'unknown' ? 'warn' : 'info'}>
            {FORMAT_LABEL[file.format]}
          </Chip>
        </LedgerCell>
        <LedgerCell>
          <Chip tone={FILE_STATUS_TONE[file.status]}>{FILE_STATUS_LABEL[file.status]}</Chip>
        </LedgerCell>
        <LedgerCell>
          <FileProgress file={file} />
        </LedgerCell>
        <LedgerCell numeric>{formatCount(file.counters.imported)}</LedgerCell>
        <LedgerCell numeric>{formatCount(file.counters.duplicates)}</LedgerCell>
        <LedgerCell numeric>{formatCount(file.counters.skipped)}</LedgerCell>
        <LedgerCell numeric>{formatCount(file.counters.rejected)}</LedgerCell>
      </LedgerRow>
      {file.errorMessage || file.errorCode ? (
        <LedgerRow>
          <LedgerCell colSpan={8}>
            <p className="text-sm text-ember">
              {file.errorMessage || failureAdvice(file.errorCode)}
            </p>
            {file.errorCode ? (
              <p className="tabular mt-1 text-xs text-ink-faint">{file.errorCode}</p>
            ) : null}
          </LedgerCell>
        </LedgerRow>
      ) : null}
    </>
  )
}

/**
 * How far through a file the importer is.
 *
 * `recordsTotal` is null until the file has been read to the end, so until then
 * there is a count of records read and the word "counting" — never a bar, and
 * never a percentage of a denominator that does not exist yet.
 */
function FileProgress({ file }: { file: ImportFile }): ReactElement {
  if (file.recordsTotal === null) {
    if (file.status === 'pending') {
      return <span className="text-xs text-ink-faint">Not started</span>
    }
    return (
      <span className="text-xs text-ink-muted">
        <span className="tabular">{formatCount(file.recordOffset)}</span> read · counting
      </span>
    )
  }

  const share =
    file.recordsTotal > 0 ? Math.min(Math.max(file.recordOffset / file.recordsTotal, 0), 1) : 1

  return (
    <div className="min-w-36">
      <div className="flex items-baseline justify-between gap-2 text-xs">
        <span className="tabular text-ink">
          {formatCount(file.recordOffset)} / {formatCount(file.recordsTotal)}
        </span>
        <span className="tabular text-ink-muted">{formatPercent(share, 0)}</span>
      </div>
      <div
        className="meter mt-1"
        role="progressbar"
        aria-label={`${file.name} progress`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(share * 100)}
        aria-valuetext={`${formatCount(file.recordOffset)} of ${formatCount(file.recordsTotal)} records read`}
      >
        <span style={{ width: `${share * 100}%` }} />
      </div>
    </div>
  )
}

/** One rejected record, laid out to be read rather than counted. */
function RejectItem({ reject }: { reject: RejectRow }): ReactElement {
  return (
    <li className="px-4 py-3">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <p className="text-sm text-ink">
          <span className="eyebrow mr-2">Record</span>
          <span className="tabular">{formatCount(reject.recordIndex)}</span>
          {reject.file ? (
            <span className="tabular ml-2 text-xs break-all text-ink-faint">{reject.file}</span>
          ) : null}
        </p>
        <Chip tone="warn">{reject.reason.replace(/_/g, ' ')}</Chip>
      </div>
      <p className="mt-1.5 text-sm text-ink-muted">{rejectAdvice(reject.reason)}</p>
      {reject.detail ? <p className="mt-1 text-sm text-ink-muted">{reject.detail}</p> : null}
      {reject.rawExcerpt ? (
        <pre className="tabular mt-2 max-w-full overflow-x-auto rounded-control border border-seam bg-chassis p-2.5 text-xs text-ink-muted">
          {reject.rawExcerpt}
        </pre>
      ) : null}
    </li>
  )
}
