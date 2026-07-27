// Package accounts holds the identity repositories: users, their Spotify grant,
// login sessions, in-flight OAuth states and instance-wide settings.
//
// The five tables move together. A single sign-in reads app_settings, upserts a
// user, writes a credential and creates a session, and all of that has to commit
// or roll back as one unit, so the repositories share a package and every method
// takes an explicit store.Querier. The same code therefore runs inside the
// sign-in transaction and, for the background reapers and the sync scheduler,
// straight against the pool.
//
// Secrets never leave this package in the clear beyond the domain types: Spotify
// tokens and PKCE verifiers are sealed with store.Seal on the way in and opened
// with store.Open on the way out, and no error message here carries a token, a
// verifier or a session cookie.
package accounts

import "github.com/RequiDev/encore/internal/store"

// Repo bundles the identity repositories so that wiring can pass one value
// around. The sub-repositories are independent and may equally be constructed on
// their own when a caller only needs one of them.
type Repo struct {
	Users       *Users
	Credentials *Credentials
	Sessions    *Sessions
	OAuthStates *OAuthStates
	Settings    *Settings
	Shares      *Shares
	Playlists   *Playlists
}

// New builds every identity repository from one store.
func New(db *store.Store) *Repo {
	return &Repo{
		Users:       NewUsers(db),
		Credentials: NewCredentials(db),
		Sessions:    NewSessions(db),
		OAuthStates: NewOAuthStates(db),
		Settings:    NewSettings(db),
		Shares:      NewShares(db),
		Playlists:   NewPlaylists(db),
	}
}

// rowScanner is the part of pgx.Row and pgx.Rows that the scan helpers need.
// Declaring it here keeps pgx out of this package's imports, which is the rule
// the store sub-packages follow: pgx types belong to internal/postgres and
// internal/store.
type rowScanner interface {
	Scan(dest ...any) error
}

// Page bounds. A listing endpoint is administrative and its caller is trusted,
// but an unbounded LIMIT is still a way to turn one request into a full table
// scan, so the repository clamps rather than trusting the argument.
const (
	defaultPageSize = 50
	maxPageSize     = 500
)

// clampPage normalises a caller's paging arguments.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// clampLimit normalises a bare row limit.
func clampLimit(limit int) int {
	n, _ := clampPage(limit, 0)
	return n
}
