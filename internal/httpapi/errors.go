package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
)

// The machine-readable error codes of docs/api.md. The frontend switches on
// these, so they are part of the contract and must not be reworded.
const (
	// CodeInvalidRequest is a malformed or out-of-range parameter.
	CodeInvalidRequest = "invalid_request"
	// CodeUnauthenticated is a missing or expired session.
	CodeUnauthenticated = "unauthenticated"
	// CodeCSRF is a missing or mismatched CSRF token.
	CodeCSRF = "csrf"
	// CodeForbidden is an authenticated caller who is not permitted.
	CodeForbidden = "forbidden"
	// CodeNotFound covers objects that exist but belong to someone else.
	CodeNotFound = "not_found"
	// CodeConflict is a state conflict, such as retrying a running job.
	CodeConflict = "conflict"
	// CodePayloadTooLarge is an upload beyond ENCORE_IMPORT_MAX_UPLOAD_BYTES.
	CodePayloadTooLarge = "payload_too_large"
	// CodeRegistrationsDisabled is an unknown Spotify identity on a closed instance.
	CodeRegistrationsDisabled = "registrations_disabled"
	// CodeAccountDisabled is a deactivated account.
	CodeAccountDisabled = "account_disabled"
	// CodeRateLimited is too many requests.
	CodeRateLimited = "rate_limited"
	// CodeInternal is an unexpected failure. Its message is deliberately vague.
	CodeInternal = "internal"
)

// vagueInternalMessage is what a 500 tells the caller. The real error goes to
// the log, where an operator can see it, and nowhere else: a database message
// can carry a constraint name, a host name or worse.
const vagueInternalMessage = "Something went wrong. The problem has been logged."

// ErrorPayload is the body of every failed request.
type ErrorPayload struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

// ErrorBody is the single error envelope described in docs/api.md.
type ErrorBody struct {
	Error ErrorPayload `json:"error"`
}

// APIError is a failure with a status and a code already decided.
//
// Handlers return it when they know better than the generic sentinel mapping —
// naming the offending query parameter, say. cause is logged but never sent.
type APIError struct {
	Status  int
	Code    string
	Message string
	Details map[string]string

	cause error
}

// Error implements error. It renders the client-facing message, so wrapping an
// APIError never leaks the cause into a response.
func (e *APIError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *APIError) Unwrap() error { return e.cause }

// WithCause attaches an underlying error for the log without changing what the
// caller is told.
func (e *APIError) WithCause(err error) *APIError {
	e.cause = err
	return e
}

// ErrInvalidRequest builds a 400 naming, through details, the field at fault.
func ErrInvalidRequest(message string, details map[string]string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: message, Details: details}
}

// ErrFieldInvalid is the common case of ErrInvalidRequest: one named parameter.
func ErrFieldInvalid(field, message string) *APIError {
	return ErrInvalidRequest(message, map[string]string{"field": field})
}

// ErrUnauthorized builds the 401 an anonymous caller receives.
func ErrUnauthorized() *APIError {
	return &APIError{
		Status:  http.StatusUnauthorized,
		Code:    CodeUnauthenticated,
		Message: "You are not signed in.",
	}
}

// ErrCSRF builds the 403 a request with a missing or mismatched token receives.
func ErrCSRF() *APIError {
	return &APIError{
		Status:  http.StatusForbidden,
		Code:    CodeCSRF,
		Message: "The CSRF token was missing or did not match. Reload the page and try again.",
	}
}

// ErrForbiddenf builds a 403 for an authenticated caller who may not do this.
func ErrForbiddenf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: CodeForbidden, Message: fmt.Sprintf(format, args...)}
}

// ErrNotFoundf builds a 404. It is also the answer for an object owned by
// somebody else, so that ids cannot be probed for existence.
func ErrNotFoundf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// ErrConflictf builds a 409.
func ErrConflictf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusConflict, Code: CodeConflict, Message: fmt.Sprintf(format, args...)}
}

// ErrTooLarge builds the 413 an oversized upload receives.
func ErrTooLarge(limit int64) *APIError {
	return &APIError{
		Status:  http.StatusRequestEntityTooLarge,
		Code:    CodePayloadTooLarge,
		Message: fmt.Sprintf("The upload is larger than the %d byte limit this instance allows.", limit),
	}
}

// ErrInternal builds the deliberately vague 500 and keeps the real cause for the
// log.
func ErrInternal(cause error) *APIError {
	return &APIError{
		Status:  http.StatusInternalServerError,
		Code:    CodeInternal,
		Message: vagueInternalMessage,
		cause:   cause,
	}
}

// sentinelMapping is the table docs/api.md documents, in the order it is
// consulted. The order matters only in that every entry is checked with
// errors.Is, so a wrapped sentinel is found however deep it is.
var sentinelMapping = []struct {
	sentinel error
	status   int
	code     string
	// fallback is used when the error carries nothing beyond the sentinel text.
	fallback string
	// verbatim allows the wrapped detail through to the caller. It is set only
	// for errors this codebase writes for the caller's benefit; anything that
	// could carry a database message stays behind a fixed sentence.
	verbatim bool
}{
	{domain.ErrValidation, http.StatusBadRequest, CodeInvalidRequest, "The request was not valid.", true},
	{domain.ErrUnauthenticated, http.StatusUnauthorized, CodeUnauthenticated, "You are not signed in.", false},
	{domain.ErrRegistrationsDisabled, http.StatusForbidden, CodeRegistrationsDisabled, "This instance is not accepting new accounts.", false},
	{domain.ErrAccountDisabled, http.StatusForbidden, CodeAccountDisabled, "This account has been deactivated.", false},
	{domain.ErrForbidden, http.StatusForbidden, CodeForbidden, "You are not allowed to do that.", true},
	{domain.ErrNotFound, http.StatusNotFound, CodeNotFound, "That does not exist.", false},
	{domain.ErrConflict, http.StatusConflict, CodeConflict, "That conflicts with the current state.", true},
}

// asAPIError turns any error into the response it deserves.
//
// Anything not in the sentinel table is a 500 with a vague message: an error
// nobody classified is by definition one nobody has decided is safe to show.
func asAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	for _, m := range sentinelMapping {
		if !errors.Is(err, m.sentinel) {
			continue
		}
		message := m.fallback
		if m.verbatim {
			if detail := detailAfter(err.Error(), m.sentinel.Error()); detail != "" {
				message = capitalise(detail)
			}
		}
		return &APIError{Status: m.status, Code: m.code, Message: message, cause: err}
	}
	return ErrInternal(err)
}

// detailAfter extracts the part of a wrapped error that follows its sentinel.
//
// Errors here are built as fmt.Errorf("%w: something specific", sentinel) and
// may then be prefixed by the operation that failed, so the useful sentence is
// whatever comes after the sentinel's own text.
func detailAfter(full, sentinel string) string {
	i := strings.LastIndex(full, sentinel)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(full[i+len(sentinel):]), ":"))
}

// capitalise makes a fragment read as a sentence without touching an identifier
// that is already capitalised or quoted.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	if last := r[len(r)-1]; last != '.' && last != '!' && last != '?' {
		r = append(r, '.')
	}
	return string(r)
}

// writeError renders err as the API's error envelope and logs it when the
// failure is ours rather than the caller's.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	lg := logging.FromContext(r.Context())

	// A client that has gone away cannot read a response, and its cancellation
	// is not a server fault, so it is logged quietly and nothing is written.
	if r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		lg.Debug("request cancelled before a response was written", logging.Err(err))
		return
	}

	apiErr := asAPIError(err)
	if apiErr.Status >= http.StatusInternalServerError {
		lg.Error("request failed", slog.String("code", apiErr.Code), logging.Err(err))
	} else {
		lg.Debug("request rejected", slog.String("code", apiErr.Code), logging.Err(err))
	}

	writeJSON(w, r, apiErr.Status, ErrorBody{Error: ErrorPayload{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Details: apiErr.Details,
	}})
}

// writeJSON serialises v and sends it.
//
// The body is encoded into memory first so that a marshalling failure becomes a
// 500 rather than a truncated 200 with a half-written object; every payload
// written this way is a bounded page, and the streaming endpoints do not use it.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		logging.FromContext(r.Context()).Error("could not encode response", logging.Err(err))
		body, status = []byte(`{"error":{"code":"internal","message":"`+vagueInternalMessage+`"}}`), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		logging.FromContext(r.Context()).Debug("could not write response body", logging.Err(err))
	}
}

// writeNoContent ends a request that has nothing to say.
func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
