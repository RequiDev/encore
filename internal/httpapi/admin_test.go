package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
)

// adminRequest builds a request already carrying an authenticated caller, so
// requireAdmin can be exercised without the session middleware in the way.
func adminRequest(user domain.User) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	ctx := context.WithValue(r.Context(), ctxKeyAuth{}, authContext{
		session: domain.Session{ID: uuid.New(), UserID: user.ID, CSRFToken: "csrf"},
		user:    user,
	})
	return r.WithContext(ctx)
}

// TestRequireAdminReadsTheRoleFromTheDatabase is the point of the guard: the
// session is not trusted, so a demotion takes effect on the next request rather
// than at the demoted administrator's next sign-in.
func TestRequireAdminReadsTheRoleFromTheDatabase(t *testing.T) {
	id := uuid.New()
	stale := domain.User{ID: id, Role: domain.RoleAdmin, IsActive: true}
	stored := domain.User{ID: id, Role: domain.RoleUser, IsActive: true}

	ts := newTestServer(t)
	ts.users.byID[id] = stored

	_, err := ts.requireAdmin(adminRequest(stale))
	apiErr := apiErrorOf(t, err)
	if apiErr.Status != http.StatusForbidden || apiErr.Code != CodeForbidden {
		t.Fatalf("status/code = %d/%s, want 403/%s", apiErr.Status, apiErr.Code, CodeForbidden)
	}
}

// TestRequireAdminAdmitsARealAdministrator is the other half of the same rule.
func TestRequireAdminAdmitsARealAdministrator(t *testing.T) {
	id := uuid.New()
	// The session says "user"; the database says "admin". The database wins.
	stale := domain.User{ID: id, Role: domain.RoleUser, IsActive: true}
	stored := domain.User{ID: id, Role: domain.RoleAdmin, IsActive: true, DisplayName: "Owner"}

	ts := newTestServer(t)
	ts.users.byID[id] = stored

	got, err := ts.requireAdmin(adminRequest(stale))
	if err != nil {
		t.Fatalf("requireAdmin refused a stored administrator: %v", err)
	}
	if got.DisplayName != "Owner" {
		t.Fatalf("requireAdmin returned %+v, want the stored row", got)
	}
}

// TestRequireAdminRefusesADeactivatedAdministrator checks that an account an
// administrator has switched off cannot keep administering.
func TestRequireAdminRefusesADeactivatedAdministrator(t *testing.T) {
	id := uuid.New()
	ts := newTestServer(t)
	ts.users.byID[id] = domain.User{ID: id, Role: domain.RoleAdmin, IsActive: false}

	_, err := ts.requireAdmin(adminRequest(domain.User{ID: id, Role: domain.RoleAdmin, IsActive: true}))
	if !errors.Is(err, domain.ErrAccountDisabled) {
		t.Fatalf("requireAdmin returned %v, want ErrAccountDisabled", err)
	}
}

// TestRequireAdminNeedsASession checks that an anonymous caller is told to sign
// in rather than told they are not an administrator.
func TestRequireAdminNeedsASession(t *testing.T) {
	ts := newTestServer(t)

	_, err := ts.requireAdmin(httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	apiErr := apiErrorOf(t, err)
	if apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", apiErr.Status)
	}
}

// TestGuardLastAdmin pins the rule that keeps an instance from being locked out
// of its own settings.
func TestGuardLastAdmin(t *testing.T) {
	admin := domain.User{ID: uuid.New(), Role: domain.RoleAdmin, IsActive: true}
	ordinary := domain.User{ID: uuid.New(), Role: domain.RoleUser, IsActive: true}
	dormant := domain.User{ID: uuid.New(), Role: domain.RoleAdmin, IsActive: false}

	cases := []struct {
		name         string
		target       domain.User
		remainsAdmin bool
		admins       int64
		wantRefused  bool
	}{
		{"demoting the last administrator", admin, false, 1, true},
		{"deleting the last administrator", admin, false, 1, true},
		{"demoting one of two administrators", admin, false, 2, false},
		{"leaving an administrator an administrator", admin, true, 1, false},
		{"deleting an ordinary user", ordinary, false, 1, false},
		{"deleting an already deactivated administrator", dormant, false, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.users.admins = c.admins

			err := ts.guardLastAdmin(context.Background(), c.target, c.remainsAdmin)
			if c.wantRefused {
				apiErr := apiErrorOf(t, err)
				if apiErr.Status != http.StatusConflict || apiErr.Code != CodeConflict {
					t.Fatalf("status/code = %d/%s, want 409/%s", apiErr.Status, apiErr.Code, CodeConflict)
				}
				return
			}
			if err != nil {
				t.Fatalf("guardLastAdmin refused a permitted change: %v", err)
			}
		})
	}
}
