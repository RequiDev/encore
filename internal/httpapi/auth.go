package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// oauthStateTTL is how long an authorisation request may take to come back. It
// is short because the row holds a PKCE verifier: the shorter it exists, the
// smaller the window in which a stolen state is worth anything.
const oauthStateTTL = 10 * time.Minute

// errIdentityMismatch is the relink flow refusing to attach a different Spotify
// account to an existing Encore user. Doing so would silently merge two
// listening histories with no way back.
var errIdentityMismatch = errors.New("the Spotify account does not match the linked one")

// OAuth failure codes. They are appended to ${ENCORE_WEB_URL}/login?error= so
// the client can explain what happened; they are stable identifiers, not prose.
const (
	oauthErrInvalidRequest = "invalid_request"
	oauthErrInvalidState   = "invalid_state"
	oauthErrExchangeFailed = "exchange_failed"
	// oauthErrSpotifyRateLimited is separate from exchange_failed because the
	// advice differs completely. "Try again" is right for a spent code and wrong
	// for a rate limit, where trying again is the one thing that will not work.
	oauthErrSpotifyRateLimited    = "spotify_rate_limited"
	oauthErrProfileFailed         = "profile_failed"
	oauthErrRegistrationsDisabled = "registrations_disabled"
	oauthErrAccountDisabled       = "account_disabled"
	oauthErrIdentityMismatch      = "identity_mismatch"
	oauthErrUnauthenticated       = "unauthenticated"
	oauthErrInternal              = "internal"
	oauthErrFailed                = "oauth_failed"
)

// handleLogin answers GET /api/auth/spotify/login by starting an authorisation
// journey for whoever is asking.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.startOAuth(w, r, nil, nil)
}

// handleRelink answers GET /api/auth/spotify/relink: the same journey, but tied
// to the signed-in account, for when Spotify has revoked a refresh token.
func (s *Server) handleRelink(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		// This is a top-level navigation, so the browser gets a page rather than
		// a JSON error it would have no way to show.
		s.redirectWithError(w, r, oauthErrUnauthenticated)
		return
	}
	s.startOAuth(w, r, &user.ID, nil)
}

// handleAuthorizePlaylists answers GET /api/auth/spotify/playlists.
//
// The same journey as a relink, asking for one extra scope. Encore's default
// grant is read-only and stays that way for anybody who never uses playlists:
// demanding write access from every listener on every instance, for a feature
// most will not touch, would be a poor trade for a statistics application.
func (s *Server) handleAuthorizePlaylists(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		s.redirectWithError(w, r, oauthErrUnauthenticated)
		return
	}
	s.startOAuth(w, r, &user.ID, []string{spotify.ScopePlaylistPrivate})
}

// startOAuth mints the state and PKCE verifier, records them, and sends the
// browser to Spotify.
//
// Only the SHA-256 of the state is stored and the verifier is sealed, so the
// row gives away neither the value an attacker would have to forge nor the one
// they would have to present.
func (s *Server) startOAuth(w http.ResponseWriter, r *http.Request, linkUserID *uuid.UUID, extraScopes []string) {
	ctx := r.Context()
	lg := logging.FromContext(ctx)

	state, err := crypto.NewToken()
	if err != nil {
		lg.Error("could not mint an oauth state", logging.Err(err))
		s.redirectWithError(w, r, oauthErrInternal)
		return
	}
	verifier, err := crypto.PKCEVerifier()
	if err != nil {
		lg.Error("could not mint a pkce verifier", logging.Err(err))
		s.redirectWithError(w, r, oauthErrInternal)
		return
	}

	// An unusable redirect target is dropped rather than refused: the journey
	// still works, it simply ends at the web client's root.
	redirectTo, _ := validateRedirect(s.cfg.Instance.WebURL, r.URL.Query().Get("redirect_to"))

	if err := s.oauthStates.Create(ctx, s.querier,
		crypto.HashToken(state), verifier, redirectTo, linkUserID, s.now().Add(oauthStateTTL)); err != nil {
		lg.Error("could not record the oauth state", logging.Err(err))
		s.redirectWithError(w, r, oauthErrInternal)
		return
	}

	http.Redirect(w, r,
		s.spotify.AuthorizeURLWithScopes(state, crypto.PKCEChallenge(verifier), extraScopes),
		http.StatusFound)
}

// handleCallback answers GET /api/auth/spotify/callback.
//
// Everything that can go wrong here ends in a redirect to the web client's login
// page with a code, not in an API error document: the caller is a browser that
// followed Spotify's redirect, and it is expecting a page.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lg := logging.FromContext(ctx)
	q := r.URL.Query()

	if denied := q.Get("error"); denied != "" {
		// Most often access_denied: the listener pressed "Cancel".
		s.redirectWithError(w, r, sanitiseOAuthCode(denied))
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		s.redirectWithError(w, r, oauthErrInvalidRequest)
		return
	}

	verifier, redirectTo, linkUserID, err := s.oauthStates.Consume(ctx, s.querier, crypto.HashToken(state))
	if err != nil {
		// Unknown, expired or already spent. All three mean the same thing to the
		// person in front of the browser: start again.
		lg.Warn("oauth callback presented an unusable state", logging.Err(err))
		s.redirectWithError(w, r, oauthErrInvalidState)
		return
	}

	token, err := s.spotify.ExchangeCode(ctx, code, verifier)
	if err != nil {
		if paused := asPaused(err); paused != nil {
			lg.Warn("sign-in refused: spotify is rate limiting this instance",
				"resumes_at", paused.Until.UTC().Format(time.RFC3339))
			s.redirectWithError(w, r, oauthErrSpotifyRateLimited)
			return
		}
		lg.Error("could not exchange the authorisation code", logging.Err(err))
		s.redirectWithError(w, r, oauthErrExchangeFailed)
		return
	}
	if token.RefreshToken == "" {
		// Without one the account could never be polled again, so this is a
		// failed sign-in rather than a degraded one.
		lg.Error("the authorisation code exchange returned no refresh token")
		s.redirectWithError(w, r, oauthErrExchangeFailed)
		return
	}

	profile, err := s.spotify.CurrentUser(ctx, token.AccessToken)
	if err != nil {
		if paused := asPaused(err); paused != nil {
			lg.Warn("sign-in refused: spotify is rate limiting this instance",
				"resumes_at", paused.Until.UTC().Format(time.RFC3339))
			s.redirectWithError(w, r, oauthErrSpotifyRateLimited)
			return
		}
		lg.Error("could not read the Spotify profile", logging.Err(err))
		s.redirectWithError(w, r, oauthErrProfileFailed)
		return
	}

	sessionToken, csrfToken, expiresAt, err := s.completeSignIn(ctx, r, profile, token, linkUserID)
	if err != nil {
		s.redirectWithError(w, r, signInErrorCode(err))
		if !isExpectedSignInFailure(err) {
			lg.Error("could not complete the sign-in", logging.Err(err))
		}
		return
	}

	s.setAuthCookies(w, sessionToken, csrfToken, expiresAt)

	target := redirectTo
	if target == "" {
		target = s.cfg.Instance.WebURL
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// completeSignIn turns a verified Spotify grant into a user, a credential and a
// session.
//
// The three writes are made in sequence rather than in one transaction, because
// this package never imports pgx and therefore cannot name a transaction type;
// the order is what makes that safe. The user is resolved first and the
// credential stored before the session, so a failure part-way through leaves at
// worst an account that has to authorise again — never a signed-in session with
// no usable grant behind it. Every step is idempotent, so the retry that the
// login page invites simply completes the job.
func (s *Server) completeSignIn(ctx context.Context, r *http.Request, profile *spotify.UserProfile, token *spotify.Token, linkUserID *uuid.UUID) (sessionToken, csrfToken string, expiresAt time.Time, err error) {
	if sessionToken, err = crypto.NewToken(); err != nil {
		return "", "", time.Time{}, fmt.Errorf("mint session token: %w", err)
	}
	if csrfToken, err = crypto.NewToken(); err != nil {
		return "", "", time.Time{}, fmt.Errorf("mint csrf token: %w", err)
	}
	expiresAt = s.now().Add(s.cfg.Security.SessionTTL)

	user, err := s.resolveSignInUser(ctx, s.querier, profile, linkUserID)
	if err != nil {
		return "", "", time.Time{}, err
	}

	scopes := token.Scopes()
	if len(scopes) == 0 {
		// Spotify omits the scope string on some responses; the grant is then
		// exactly the one that was asked for.
		scopes = s.cfg.Spotify.Scopes
	}
	if err := s.credentials.Upsert(ctx, s.querier, domain.SpotifyCredentials{
		UserID:         user.ID,
		AccessToken:    token.AccessToken,
		RefreshToken:   token.RefreshToken,
		TokenExpiresAt: token.ExpiresAt,
		Scopes:         scopes,
		SyncState:      domain.SyncStateOK,
	}); err != nil {
		return "", "", time.Time{}, err
	}

	if _, err := s.sessions.Create(ctx, s.querier, user.ID,
		crypto.HashToken(sessionToken), csrfToken, expiresAt, userAgent(r), s.clientIP(r)); err != nil {
		return "", "", time.Time{}, err
	}

	// A session already in the browser is retired rather than left behind, so a
	// fixated session identifier cannot survive a fresh sign-in. It is removed
	// after the replacement exists, so a failure here never signs anyone out.
	if previous, ok := authFrom(ctx); ok {
		if err := s.sessions.Delete(ctx, s.querier, previous.session.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			logging.FromContext(ctx).Warn("could not retire the previous session", logging.Err(err))
		}
		s.touched.forget(previous.session.ID)
	}

	return sessionToken, csrfToken, expiresAt, nil
}

// resolveSignInUser finds or creates the account behind a Spotify profile.
//
// In the relink flow the account is already known, and the only question is
// whether the identity that just authorised is the same one: attaching a
// different Spotify account to an existing Encore user would merge two people's
// listening histories with no way to separate them again.
func (s *Server) resolveSignInUser(ctx context.Context, q store.Querier, profile *spotify.UserProfile, linkUserID *uuid.UUID) (domain.User, error) {
	if linkUserID != nil {
		user, err := s.users.GetByID(ctx, q, *linkUserID)
		if err != nil {
			return domain.User{}, err
		}
		if user.SpotifyUserID != profile.ID {
			return domain.User{}, errIdentityMismatch
		}
		if !user.IsActive {
			return domain.User{}, domain.ErrAccountDisabled
		}
		return user, nil
	}

	allowRegistration, err := s.settings.RegistrationsEnabled(ctx, q)
	if err != nil {
		return domain.User{}, err
	}
	displayName := strings.TrimSpace(profile.DisplayName)
	if displayName == "" {
		displayName = profile.ID
	}
	user, _, err := s.users.UpsertFromSpotify(ctx, q, accounts.SpotifyProfile{
		SpotifyUserID: profile.ID,
		DisplayName:   displayName,
		Email:         profile.Email,
		AvatarURL:     profile.AvatarURL(),
		Product:       profile.Product,
	}, s.cfg.Instance.DefaultTimezone, allowRegistration)
	if err != nil {
		return domain.User{}, err
	}
	return user, nil
}

// signInErrorCode maps a sign-in failure onto the code the login page shows.
func signInErrorCode(err error) string {
	switch {
	case errors.Is(err, errIdentityMismatch):
		return oauthErrIdentityMismatch
	case errors.Is(err, domain.ErrRegistrationsDisabled):
		return oauthErrRegistrationsDisabled
	case errors.Is(err, domain.ErrAccountDisabled):
		return oauthErrAccountDisabled
	default:
		return oauthErrInternal
	}
}

// isExpectedSignInFailure reports whether a failure is a policy decision rather
// than a fault, so that a closed instance does not fill the log with errors.
func isExpectedSignInFailure(err error) bool {
	return errors.Is(err, errIdentityMismatch) ||
		errors.Is(err, domain.ErrRegistrationsDisabled) ||
		errors.Is(err, domain.ErrAccountDisabled)
}

// redirectWithError ends a failed journey on the web client's login page.
func (s *Server) redirectWithError(w http.ResponseWriter, r *http.Request, code string) {
	target := strings.TrimRight(s.cfg.Instance.WebURL, "/") + "/login?error=" + url.QueryEscape(code)
	http.Redirect(w, r, target, http.StatusFound)
}

// sanitiseOAuthCode reduces whatever Spotify put in the error parameter to
// something safe to place in a URL and in a log line.
func sanitiseOAuthCode(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 40 {
		return oauthErrFailed
	}
	for i := range v {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return oauthErrFailed
		}
	}
	return v
}

// handleLogout answers POST /api/auth/logout.
//
// The session row is deleted rather than merely forgotten by the browser, so a
// cookie copied elsewhere stops working immediately. Signing out when already
// signed out is not an error.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if auth, ok := authFrom(r.Context()); ok {
		if err := s.sessions.Delete(r.Context(), s.querier, auth.session.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			writeError(w, r, err)
			return
		}
		s.touched.forget(auth.session.ID)
	}
	s.clearAuthCookies(w)
	writeNoContent(w)
}

// asPaused reports whether a failure was Spotify holding this application back,
// rather than anything about the request or the person making it.
//
// The distinction reaches the browser: a spent authorisation code and a rate
// limit both fail a sign-in, but "try again" fixes the first and is useless
// advice for the second.
func asPaused(err error) *spotify.PausedError {
	var paused *spotify.PausedError
	if errors.As(err, &paused) {
		return paused
	}
	return nil
}
