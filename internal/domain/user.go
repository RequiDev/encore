package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Role is the authorisation level of a user.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

func (r Role) Valid() bool { return r == RoleUser || r == RoleAdmin }

// IsAdmin is the single place that decides administrative privilege.
func (r Role) IsAdmin() bool { return r == RoleAdmin }

// User is an Encore account. Identity comes exclusively from Spotify: there are
// no local passwords, and SpotifyUserID is the natural key.
type User struct {
	ID            uuid.UUID
	SpotifyUserID string
	DisplayName   string
	Email         string
	AvatarURL     string
	Product       string
	Role          Role
	IsActive      bool
	Timezone      string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastLoginAt   *time.Time
}

// Location resolves the user's timezone, falling back to UTC when the stored name
// is not present in the runtime's tzdata. Statistics bucket in this location.
func (u User) Location() *time.Location {
	if u.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(u.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ValidateTimezone checks that a timezone name is loadable before it is stored.
func ValidateTimezone(name string) error {
	if name == "" {
		return fmt.Errorf("%w: timezone must not be empty", ErrValidation)
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("%w: unknown timezone %q", ErrValidation, name)
	}
	return nil
}

// SyncState describes the health of a user's Spotify connection.
type SyncState string

const (
	// SyncStateOK means the last poll succeeded.
	SyncStateOK SyncState = "ok"
	// SyncStateNeedsReauth means the refresh token was rejected; only the user can
	// fix this, by going through OAuth again.
	SyncStateNeedsReauth SyncState = "needs_reauth"
	// SyncStateError means the last poll failed for a reason that may resolve itself.
	SyncStateError SyncState = "error"
)

func (s SyncState) Valid() bool {
	switch s {
	case SyncStateOK, SyncStateNeedsReauth, SyncStateError:
		return true
	}
	return false
}

// SpotifyCredentials holds a user's OAuth grant and sync bookkeeping. It is stored
// apart from User so a revoked grant can be cleared without touching history.
//
// AccessToken and RefreshToken are plaintext in memory only; the store encrypts
// them with AES-256-GCM before they reach the database.
type SpotifyCredentials struct {
	UserID         uuid.UUID
	AccessToken    string
	RefreshToken   string
	TokenExpiresAt time.Time
	Scopes         []string
	SyncState      SyncState
	SyncCursorAt   *time.Time
	LastSyncAt     *time.Time
	LastSyncError  string
	ConnectedAt    time.Time
}

// AccessTokenValid reports whether the access token can still be used, leaving a
// safety margin so a token does not expire mid-request.
func (c SpotifyCredentials) AccessTokenValid(now time.Time) bool {
	const margin = 60 * time.Second
	return c.AccessToken != "" && now.Add(margin).Before(c.TokenExpiresAt)
}

// HasScope reports whether the grant includes a scope.
func (c SpotifyCredentials) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Session is a server-side login session. The raw token lives only in the user's
// cookie; the database stores its SHA-256 so a database leak cannot be replayed.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	CSRFToken  string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastSeenAt time.Time
	UserAgent  string
	IP         string
}

// Expired reports whether the session may no longer be used.
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// Settings keys stored in app_settings.
const (
	// SettingRegistrationsEnabled gates account creation for unknown Spotify identities.
	SettingRegistrationsEnabled = "registrations_enabled"
)
