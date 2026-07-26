package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/requi/encore/internal/domain"
)

// TestErrorMapping is the table in docs/api.md, asserted directly. The frontend
// switches on these codes, so a change here is a change to the contract.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"validation", fmt.Errorf("%w: 'from' must be before 'to'", domain.ErrValidation), http.StatusBadRequest, CodeInvalidRequest},
		{"unauthenticated", domain.ErrUnauthenticated, http.StatusUnauthorized, CodeUnauthenticated},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden, CodeForbidden},
		{"registrations closed", domain.ErrRegistrationsDisabled, http.StatusForbidden, CodeRegistrationsDisabled},
		{"account disabled", domain.ErrAccountDisabled, http.StatusForbidden, CodeAccountDisabled},
		{"not found", fmt.Errorf("get import job: %w", domain.ErrNotFound), http.StatusNotFound, CodeNotFound},
		{"conflict", fmt.Errorf("%w: import job is completed", domain.ErrConflict), http.StatusConflict, CodeConflict},
		{"transient database failure", domain.Transient("insert listens", errors.New("connection reset")), http.StatusInternalServerError, CodeInternal},
		{"anything unclassified", errors.New("something nobody thought about"), http.StatusInternalServerError, CodeInternal},
		{"an explicit api error", ErrTooLarge(1024), http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{"csrf", ErrCSRF(), http.StatusForbidden, CodeCSRF},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil), c.err)

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want JSON", got)
			}

			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not an error envelope: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, c.wantCode)
			}
			if body.Error.Message == "" {
				t.Fatal("the envelope carries no message")
			}
		})
	}
}

// TestInternalErrorsStayVague checks that a 500 says nothing about what actually
// failed: a database message can name a host, a constraint or a query.
func TestInternalErrorsStayVague(t *testing.T) {
	secret := `pq: password authentication failed for user "encore" on 10.0.0.4`

	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil), errors.New(secret))

	if body := rec.Body.String(); strings.Contains(body, "password") || strings.Contains(body, "10.0.0.4") {
		t.Fatalf("the 500 leaked its cause: %s", body)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not an error envelope: %v", err)
	}
	if body.Error.Message != vagueInternalMessage {
		t.Fatalf("message = %q, want the fixed vague sentence", body.Error.Message)
	}
}

// TestValidationMessagesKeepTheirDetail checks the other half of the bargain:
// an error written for the caller reaches them, so a bad parameter says which.
func TestValidationMessagesKeepTheirDetail(t *testing.T) {
	err := fmt.Errorf("set user timezone: %w: unknown timezone %q", domain.ErrValidation, "Mars/Olympus_Mons")

	apiErr := asAPIError(err)
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "Mars/Olympus_Mons") {
		t.Fatalf("message = %q, want it to name the offending timezone", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "set user timezone") {
		t.Fatalf("message = %q, want the internal operation prefix stripped", apiErr.Message)
	}
}

// TestDetailsSurviveIntoTheEnvelope checks that a field-level 400 tells the
// client which parameter to fix.
func TestDetailsSurviveIntoTheEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodGet, "/api/stats/timeline", nil),
		ErrFieldInvalid("interval", `"interval" must be one of hour, day, week, month or year.`))

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not an error envelope: %v", err)
	}
	if body.Error.Details["field"] != "interval" {
		t.Fatalf("details = %v, want the field named", body.Error.Details)
	}
}
