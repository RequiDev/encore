package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors shared by the store and service layers.
var (
	// ErrNotFound is returned when a requested entity does not exist, or exists
	// but is not visible to the caller. Handlers map it to 404.
	ErrNotFound = errors.New("not found")
	// ErrConflict signals a uniqueness or state conflict. Handlers map it to 409.
	ErrConflict = errors.New("conflict")
	// ErrForbidden signals a failed authorisation check. Handlers map it to 403.
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthenticated signals a missing or invalid session. Handlers map it to 401.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrRegistrationsDisabled is returned when an unknown Spotify identity
	// completes OAuth while the instance is closed to new users.
	ErrRegistrationsDisabled = errors.New("registrations are disabled on this instance")
	// ErrAccountDisabled is returned when a deactivated user signs in.
	ErrAccountDisabled = errors.New("account is disabled")
	// ErrValidation signals bad caller input. Handlers map it to 400.
	ErrValidation = errors.New("invalid request")
)

// RejectReason classifies a record that can never succeed, however many times it
// is retried. Rejected records are recorded with diagnostics and the import
// continues; they never fail the job.
type RejectReason string

const (
	RejectMalformedRecord     RejectReason = "malformed_record"
	RejectMissingTimestamp    RejectReason = "missing_timestamp"
	RejectInvalidTimestamp    RejectReason = "invalid_timestamp"
	RejectTimestampOutOfRange RejectReason = "timestamp_out_of_range"
	RejectMissingTrack        RejectReason = "missing_track_identity"
	RejectInvalidMsPlayed     RejectReason = "invalid_ms_played"
	RejectUnknownShape        RejectReason = "unrecognised_record_shape"
)

// RejectError is a permanent, per-record failure.
type RejectError struct {
	Reason RejectReason
	Detail string
}

func (e *RejectError) Error() string {
	if e.Detail == "" {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Detail)
}

// AsReject extracts a RejectError from an error chain.
func AsReject(err error) (*RejectError, bool) {
	var re *RejectError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// SkipReason classifies a well-formed record that is intentionally not stored as
// a music listen. Skips are normal and are counted separately from rejects.
type SkipReason string

const (
	// SkipNotMusic covers podcast episodes, audiobook chapters and video entries,
	// which modern Spotify exports interleave with music.
	SkipNotMusic SkipReason = "not_music"
	// SkipBelowMinimum covers plays shorter than the configured threshold.
	SkipBelowMinimum SkipReason = "below_min_ms"
	// SkipLocalFile covers `spotify:local:` entries, which have no catalogue identity.
	SkipLocalFile SkipReason = "local_file"
	// SkipBlacklisted covers listens whose artist the user has blacklisted.
	SkipBlacklisted SkipReason = "blacklisted_artist"
)

// SkipError is a permanent, per-record decision not to store the record.
type SkipError struct {
	Reason SkipReason
	Detail string
}

func (e *SkipError) Error() string {
	if e.Detail == "" {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Detail)
}

// AsSkip extracts a SkipError from an error chain.
func AsSkip(err error) (*SkipError, bool) {
	var se *SkipError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// TransientError marks a failure that is worth retrying: a dropped database
// connection, a serialisation failure, an upstream 5xx. The importer retries these
// with bounded exponential backoff before escalating to a job failure.
type TransientError struct {
	Op  string
	Err error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient failure in %s: %v", e.Op, e.Err)
}
func (e *TransientError) Unwrap() error { return e.Err }

// Transient wraps err as retryable.
func Transient(op string, err error) error {
	if err == nil {
		return nil
	}
	return &TransientError{Op: op, Err: err}
}

// IsTransient reports whether an error chain is marked retryable.
func IsTransient(err error) bool {
	var te *TransientError
	return errors.As(err, &te)
}
