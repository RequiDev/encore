package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenExpiryMargin is how early a token is treated as expired, so that one
// handed to a request does not die while that request is in flight.
const TokenExpiryMargin = 60 * time.Second

// defaultTokenTTL is assumed when Spotify omits expires_in, which it should
// never do; an hour is the value it has always sent.
const defaultTokenTTL = time.Hour

// Token is an OAuth grant as Encore holds it.
//
// RefreshToken is empty on a refresh response: Spotify only re-issues one when
// it has rotated, and the caller keeps the token it already had. Storing the
// empty value would revoke the account's ability to sync.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	ExpiresAt    time.Time
}

// Valid reports whether the access token can still be used at now.
func (t Token) Valid(now time.Time) bool {
	return t.AccessToken != "" && !t.Expired(now)
}

// Expired reports whether the token is past its life, including the safety
// margin.
func (t Token) Expired(now time.Time) bool {
	if t.ExpiresAt.IsZero() {
		return true
	}
	return !now.Add(TokenExpiryMargin).Before(t.ExpiresAt)
}

// Scopes splits the space-separated scope string Spotify returns.
func (t Token) Scopes() []string { return strings.Fields(t.Scope) }

// tokenResponse is the wire shape of every grant type.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// AuthorizeURL builds the URL a listener is sent to in order to grant access.
//
// This is the authorization code flow with PKCE: state ties the callback to the
// browser that started the journey, and codeChallenge is the S256 digest of the
// verifier the caller stored. Encore is a confidential client, so PKCE is belt
// and braces rather than a requirement, and that is exactly why it is used: it
// closes the code interception window even if the redirect is mishandled.
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("state", state)
	if len(c.cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	if codeChallenge != "" {
		q.Set("code_challenge_method", "S256")
		q.Set("code_challenge", codeChallenge)
	}
	return c.authBaseURL() + "/authorize?" + q.Encode()
}

// ExchangeCode redeems an authorization code for a token pair.
// It is an interactive call: somebody is watching a browser tab while it runs.
func (c *Client) ExchangeCode(ctx context.Context, code, codeVerifier string) (*Token, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("spotify: authorization code is empty")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	form.Set("client_id", c.cfg.ClientID)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return c.token(ctx, "exchange authorization code", form, true)
}

// RefreshToken exchanges a refresh token for a fresh access token.
//
// The returned Token usually has an empty RefreshToken, because Spotify only
// sends a new one when it rotates: the caller keeps the one it already holds.
// A rejected grant comes back wrapped in ErrInvalidGrant so the account can be
// marked needs_reauth instead of being polled for ever.
//
// Background: this runs inside the sync poller, which has all day. The one path
// where a person waits on it — a manual sync — is refused up front by the API
// while a pause is in force, rather than queueing here.
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (*Token, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("spotify: refresh token is empty")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.cfg.ClientID)
	return c.token(ctx, "refresh access token", form, false)
}

// ClientCredentialsToken obtains an application token, which carries no user
// scope and can therefore read the catalogue but nothing personal.
func (c *Client) ClientCredentialsToken(ctx context.Context) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	return c.token(ctx, "obtain application token", form, false)
}

// token posts a grant to the accounts service and normalises the response.
//
// interactive says whether a person is waiting. It matters here more than
// anywhere: this is accounts.spotify.com, a different service from the API, and
// a catalogue quota exhausted on api.spotify.com is no reason at all to refuse
// somebody a token.
func (c *Client) token(ctx context.Context, label string, form url.Values, interactive bool) (*Token, error) {
	var tr tokenResponse
	err := c.do(ctx, request{
		method:      http.MethodPost,
		url:         c.tokenURL(),
		label:       label,
		basic:       true,
		form:        form,
		out:         &tr,
		interactive: interactive,
	})
	if err != nil {
		if apiErr, ok := AsAPIError(err); ok && oauthErrorCode(apiErr.Body) == "invalid_grant" {
			// Only the listener can fix this, by authorising again. Distinguishing it
			// here is what stops a revoked account being retried for ever.
			return nil, fmt.Errorf("%w: %w", ErrInvalidGrant, apiErr)
		}
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("%s: response carried no access token", label)
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ExpiresAt:    c.clock.Now().Add(ttl).UTC(),
	}, nil
}

// appTokenCache holds the client-credentials token and the refresh in flight.
type appTokenCache struct {
	mu     sync.Mutex
	token  *Token
	flight *tokenFlight
}

// tokenFlight is one in-progress refresh. Waiters read token and err after done
// is closed, which is what makes the handover race-free.
type tokenFlight struct {
	done  chan struct{}
	token *Token
	err   error
}

// AppToken returns a valid application access token, fetching one when the
// cached token is missing or close to expiry.
//
// Refreshes are single-flight: concurrent callers wait for the one request in
// progress rather than each asking for their own token. Catalogue reads use
// this, so enrichment keeps working on an instance where nobody is connected.
func (c *Client) AppToken(ctx context.Context) (string, error) {
	now := c.clock.Now()

	c.app.mu.Lock()
	if t := c.app.token; t != nil && t.Valid(now) {
		token := t.AccessToken
		c.app.mu.Unlock()
		return token, nil
	}
	if f := c.app.flight; f != nil {
		c.app.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return "", f.err
			}
			return f.token.AccessToken, nil
		case <-ctx.Done():
			// The leader keeps going for whoever else is waiting; only this caller
			// gives up.
			return "", ctx.Err()
		}
	}
	f := &tokenFlight{done: make(chan struct{})}
	c.app.flight = f
	c.app.mu.Unlock()

	token, err := c.ClientCredentialsToken(ctx)

	c.app.mu.Lock()
	if err == nil {
		c.app.token = token
	}
	c.app.flight = nil
	c.app.mu.Unlock()

	f.token, f.err = token, err
	close(f.done)

	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

// InvalidateAppToken drops the cached application token so the next call
// fetches a fresh one. Used when Spotify rejects a token that had not yet
// reached its stated expiry.
func (c *Client) InvalidateAppToken() {
	c.app.mu.Lock()
	defer c.app.mu.Unlock()
	c.app.token = nil
}

// invalidateAppTokenIf clears the cache only when it still holds the token that
// was rejected, so a refresh that has already happened is not thrown away.
func (c *Client) invalidateAppTokenIf(rejected string) {
	c.app.mu.Lock()
	defer c.app.mu.Unlock()
	if c.app.token != nil && c.app.token.AccessToken == rejected {
		c.app.token = nil
	}
}
