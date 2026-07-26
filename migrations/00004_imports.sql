-- +goose Up

-- An import job is a user-initiated ingestion of one or more streaming-history
-- files. Jobs are claimed by a worker under a lease, so a crashed worker's job
-- becomes claimable again without any operator involvement.
CREATE TABLE import_jobs (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status           text        NOT NULL DEFAULT 'queued'
                                 CHECK (status IN ('queued', 'running', 'paused', 'completed', 'failed', 'cancelled')),
    note             text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    error_code       text        NOT NULL DEFAULT '',
    error_message    text        NOT NULL DEFAULT '',
    -- Lease held by the worker currently processing the job. When
    -- lease_expires_at passes without a heartbeat the job is reclaimable.
    lease_owner      text        NOT NULL DEFAULT '',
    lease_expires_at timestamptz,
    -- Set by the API; observed by the worker at each batch boundary.
    cancel_requested boolean     NOT NULL DEFAULT false,
    files_total      integer     NOT NULL DEFAULT 0,
    files_done       integer     NOT NULL DEFAULT 0
);

-- Job claiming: "queued, or running with an expired lease", oldest first.
CREATE INDEX import_jobs_claim_idx ON import_jobs (created_at)
    WHERE status IN ('queued', 'running');
CREATE INDEX import_jobs_user_idx ON import_jobs (user_id, created_at DESC);

-- One row per streaming-history file. Carries the durable checkpoint.
CREATE TABLE import_files (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id         uuid        NOT NULL REFERENCES import_jobs (id) ON DELETE CASCADE,
    ordinal        integer     NOT NULL DEFAULT 0,
    name           text        NOT NULL,
    -- Entry path when this file was found inside an uploaded .zip archive.
    container_path text        NOT NULL DEFAULT '',
    format         text        NOT NULL DEFAULT 'unknown'
                               CHECK (format IN ('extended', 'account_data', 'unknown')),
    size_bytes     bigint      NOT NULL DEFAULT 0,
    -- SHA-256 of the file's bytes, used to tell the user they have already
    -- imported this exact file. Content is still re-read: the dedupe keys, not
    -- this hash, are what guarantee idempotency.
    sha256         bytea,
    storage_path   text        NOT NULL DEFAULT '',
    status         text        NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'running', 'completed', 'failed', 'skipped')),

    -- Known only once the file has been read to the end. NULL means the UI shows
    -- "pending: unknown" rather than inventing a denominator.
    records_total  bigint,

    -- The checkpoint. Written inside the same transaction as the batch it
    -- describes, so "committed records <= checkpoint" is always exactly true.
    record_offset  bigint      NOT NULL DEFAULT 0,
    -- Decoder input offset after the last accounted record. NULL for
    -- non-seekable sources (gzip streams, zip entries), which resume by
    -- replaying and discarding record_offset records instead.
    byte_offset    bigint,

    imported     bigint NOT NULL DEFAULT 0,
    duplicates   bigint NOT NULL DEFAULT 0,
    skipped      bigint NOT NULL DEFAULT 0,
    rejected     bigint NOT NULL DEFAULT 0,

    error_code    text        NOT NULL DEFAULT '',
    error_message text        NOT NULL DEFAULT '',
    started_at    timestamptz,
    finished_at   timestamptz,

    CONSTRAINT import_files_job_ordinal_uk UNIQUE (job_id, ordinal)
);

CREATE INDEX import_files_job_idx ON import_files (job_id, ordinal);
-- "Have I imported this file before?" across all of a user's jobs.
CREATE INDEX import_files_sha_idx ON import_files (sha256) WHERE sha256 IS NOT NULL;

-- Diagnostics for records that can never succeed. Capped per file by the
-- importer so one pathological export cannot fill the disk.
CREATE TABLE import_rejects (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    file_id      uuid        NOT NULL REFERENCES import_files (id) ON DELETE CASCADE,
    record_index bigint      NOT NULL,
    reason       text        NOT NULL,
    detail       text        NOT NULL DEFAULT '',
    -- A truncated copy of the offending record so a user can see what was wrong
    -- without re-opening a multi-gigabyte export.
    raw_excerpt  text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX import_rejects_file_idx ON import_rejects (file_id, record_index);

-- +goose Down
DROP TABLE import_rejects;
DROP TABLE import_files;
DROP TABLE import_jobs;
