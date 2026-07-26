package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/logging"
)

// requireAdmin is the guard on every administrative route.
//
// It re-reads the role from the database rather than trusting anything carried
// in the session, so a demotion takes effect on the very next request instead of
// whenever the demoted administrator happens to sign in again.
func (s *Server) requireAdmin(r *http.Request) (domain.User, error) {
	caller, err := requireUser(r)
	if err != nil {
		return domain.User{}, err
	}
	fresh, err := s.users.GetByID(r.Context(), s.querier, caller.ID)
	if err != nil {
		return domain.User{}, err
	}
	if !fresh.IsActive {
		return domain.User{}, domain.ErrAccountDisabled
	}
	if !fresh.Role.IsAdmin() {
		return domain.User{}, ErrForbiddenf("This action needs administrator access.")
	}
	return fresh, nil
}

// handleGetSettings answers GET /api/admin/settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeError(w, r, err)
		return
	}
	enabled, err := s.settings.RegistrationsEnabled(r.Context(), s.querier)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, AdminSettings{RegistrationsEnabled: enabled})
}

// updateSettingsRequest is the body of PATCH /api/admin/settings.
type updateSettingsRequest struct {
	RegistrationsEnabled *bool `json:"registrationsEnabled"`
}

// handleUpdateSettings answers PATCH /api/admin/settings.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body updateSettingsRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.RegistrationsEnabled == nil {
		writeError(w, r, ErrFieldInvalid("registrationsEnabled", `"registrationsEnabled" is required.`))
		return
	}
	ctx := r.Context()

	if err := s.settings.SetRegistrationsEnabled(ctx, s.querier, *body.RegistrationsEnabled); err != nil {
		writeError(w, r, err)
		return
	}
	enabled, err := s.settings.RegistrationsEnabled(ctx, s.querier)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, AdminSettings{RegistrationsEnabled: enabled})
}

// handleAdminListUsers answers GET /api/admin/users.
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	users, total, err := s.users.ListUsers(ctx, s.querier, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]AdminUser, 0, len(users))
	for _, u := range users {
		entry := AdminUser{User: toUser(u), SyncState: string(domain.SyncStateNeedsReauth)}

		count, err := s.listens.CountListensForUser(ctx, s.querier, u.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		entry.ListenCount = count

		creds, err := s.credentials.Get(ctx, s.querier, u.ID)
		switch {
		case err == nil:
			entry.SyncState = string(creds.SyncState)
			entry.LastSyncAt = tsPtr(creds.LastSyncAt)
		case errors.Is(err, domain.ErrNotFound):
			// An account that has never connected keeps the needs_reauth default,
			// which is what an administrator has to act on anyway.
		default:
			writeError(w, r, err)
			return
		}
		items = append(items, entry)
	}
	writeJSON(w, r, http.StatusOK, Page[AdminUser]{Items: items, Total: total})
}

// updateUserRequest is the body of PATCH /api/admin/users/{id}.
type updateUserRequest struct {
	Role     *string `json:"role"`
	IsActive *bool   `json:"isActive"`
}

// handleAdminUpdateUser answers PATCH /api/admin/users/{id}.
func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	admin, err := s.requireAdmin(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var body updateUserRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Role == nil && body.IsActive == nil {
		writeError(w, r, ErrInvalidRequest(`Supply "role", "isActive" or both.`, nil))
		return
	}
	ctx := r.Context()

	target, err := s.users.GetByID(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	role := target.Role
	if body.Role != nil {
		role = domain.Role(*body.Role)
		if !role.Valid() {
			writeError(w, r, ErrFieldInvalid("role", `"role" must be user or admin.`))
			return
		}
	}
	active := target.IsActive
	if body.IsActive != nil {
		active = *body.IsActive
	}

	// The two changes are considered together: demoting and deactivating in one
	// request must not slip past a check that only looked at one of them.
	if err := s.guardLastAdmin(ctx, target, role.IsAdmin() && active); err != nil {
		writeError(w, r, err)
		return
	}

	updated := target
	if body.Role != nil && role != target.Role {
		if updated, err = s.users.SetRole(ctx, s.querier, id, role); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if body.IsActive != nil && active != target.IsActive {
		if updated, err = s.users.SetActive(ctx, s.querier, id, active); err != nil {
			writeError(w, r, err)
			return
		}
	}
	s.logAdminAction(ctx, "update_user", admin.ID, id)
	writeJSON(w, r, http.StatusOK, toUser(updated))
}

// handleAdminDeleteUser answers DELETE /api/admin/users/{id}.
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	admin, err := s.requireAdmin(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	target, err := s.users.GetByID(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.guardLastAdmin(ctx, target, false); err != nil {
		writeError(w, r, err)
		return
	}

	// As with self-deletion, the spooled uploads are removed before the rows
	// that point at them cascade away.
	s.removeAllImportFiles(r, target)

	if err := s.users.DeleteUser(ctx, s.querier, id); err != nil {
		writeError(w, r, err)
		return
	}
	s.logAdminAction(ctx, "delete_user", admin.ID, id)
	writeNoContent(w)
}

// guardLastAdmin refuses a change that would leave the instance with no
// administrator who could sign in and undo it.
//
// remainsAdmin says whether the target would still be an active administrator
// afterwards; deletion passes false. Only users who are administrators *now*
// are counted, and only active ones, because a deactivated administrator cannot
// rescue anything.
func (s *Server) guardLastAdmin(ctx context.Context, target domain.User, remainsAdmin bool) error {
	if !target.Role.IsAdmin() || !target.IsActive || remainsAdmin {
		return nil
	}
	admins, err := s.users.CountAdmins(ctx, s.querier)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return ErrConflictf("This is the last administrator; promote somebody else first.")
	}
	return nil
}

// logAdminAction records an administrative change with the actor and the
// subject, which is the audit trail a single-operator instance needs.
func (s *Server) logAdminAction(ctx context.Context, action string, actor, subject uuid.UUID) {
	logging.FromContext(ctx).Info("administrative action",
		"action", action, "actor", actor.String(), "subject", subject.String())
}
