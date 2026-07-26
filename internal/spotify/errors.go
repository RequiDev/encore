package spotify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidGrant reports that Spotify refused an authorisation grant: the code
// was already redeemed, or the listener revoked the refresh token.
//
// It is exported because it is the one OAuth failure that no amount of retrying
// can fix. Callers mark the account needs_reauth and stop polling it, rather
// than burning quota on a grant that will never work again.
var ErrInvalidGrant = errors.New("spotify: authorisation grant is no longer valid")

// APIError is a non-2xx response from Spotify.
//
// Body is the raw response, truncated, kept as data rather than folded into
// Error() so that a caller decides for itself whether an upstream payload
// belongs in its logs.
type APIError struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
	Body       string
}

// Error renders the failure. It carries only the status and Spotify's own
// message: no header, no request body and therefore no access token, refresh
// token or client secret can reach a log line or an API response through here.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if msg == "" {
		msg = "request failed"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("spotify api: status %d: %s (retry after %s)", e.StatusCode, msg, e.RetryAfter)
	}
	return fmt.Sprintf("spotify api: status %d: %s", e.StatusCode, msg)
}

// IsNotFound reports a 404: the id does not exist, or does not exist in the
// market the token resolves to. Enrichment records these as unavailable.
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsUnauthorized reports a 401: the token is missing, expired or revoked.
func (e *APIError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports a 403: the token is valid but lacks the scope, or the
// application is not allowed to read the resource.
func (e *APIError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsRateLimited reports a 429.
func (e *APIError) IsRateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// IsServerError reports a 5xx, which is worth another attempt.
func (e *APIError) IsServerError() bool { return e.StatusCode >= 500 }

// AsAPIError extracts an APIError from an error chain.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// maxErrorBodyBytes bounds how much of a failure response is kept. Enough to
// diagnose, far too little to matter if an upstream misbehaves.
const maxErrorBodyBytes = 4 << 10

// errorEnvelope covers both shapes Spotify uses. The Web API answers with
// {"error":{"status":404,"message":"..."}}, while the accounts service answers
// with the OAuth shape {"error":"invalid_grant","error_description":"..."}.
type errorEnvelope struct {
	Error            json.RawMessage `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

// errorMessage pulls a human-readable message out of an error body, tolerating
// either shape and any amount of nonsense from an intercepting proxy.
func errorMessage(body []byte) string {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Error) == 0 {
		return ""
	}

	var obj struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(env.Error, &obj); err == nil && obj.Message != "" {
		if obj.Reason != "" {
			return obj.Message + " (" + obj.Reason + ")"
		}
		return obj.Message
	}

	var code string
	if err := json.Unmarshal(env.Error, &code); err == nil && code != "" {
		if env.ErrorDescription != "" {
			return code + ": " + env.ErrorDescription
		}
		return code
	}
	return ""
}

// oauthErrorCode returns the RFC 6749 error code from a token endpoint failure,
// or "" when the body is not in that shape.
func oauthErrorCode(body string) string {
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil || len(env.Error) == 0 {
		return ""
	}
	var code string
	if err := json.Unmarshal(env.Error, &code); err != nil {
		return ""
	}
	return strings.TrimSpace(code)
}

// parseRetryAfter reads a Retry-After header, which RFC 9110 allows to be either
// a delay in seconds or an HTTP-date. The second return value distinguishes
// "absent or unparseable" from "present and zero".
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
