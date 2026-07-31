package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/playlistcover"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
)

// These tests cover the one endpoint in Encore that changes something a
// listener already has on their own Spotify account. Everything else this
// project writes either reads, or creates a new object that did not exist
// before; a rename modifies theirs, and there is nothing to re-fetch if it goes
// wrong. So the assertions here are about two things and not about coverage:
// the order the two systems are written in, and whether each sentence Encore
// sends back is a claim it is in a position to make.

// --- fixtures ---------------------------------------------------------------

const (
	// storedSpotifyID is the id on the stored row. It is the only id a rename may
	// ever be sent to.
	storedSpotifyID = "storedplaylist000001"
	// bodySpotifyID is what a caller puts in the request body hoping Encore will
	// use it instead of the stored one. Nothing may ever send it.
	bodySpotifyID = "someoneelsesplaylist"
	// fixtureName is what the playlist is called before anybody renames it.
	fixtureName = "Heavy rotation"
	// wantDescription is what Describe renders for the fixture definition below.
	//
	// Written out rather than computed from the definition: comparing the
	// handler's output against another call to the same function would pass
	// however the sentence changed. Two of this phase's defects are visible in
	// this one string. The range is half-open, so a To of 1 January 2026 has 31
	// December 2025 as its last included instant and must read as such; and the
	// limit is applied to every mode, min_plays included, so the count is a
	// ceiling ("Up to 50") rather than a promise ("Every track").
	wantDescription = "Up to 50 tracks you played at least 3 times between 1 January 2025 " +
		"and 31 December 2025, ranked by play count. Built by Encore on 26 July 2026."
)

// fixtureDefinition is deliberately a ranged min_plays definition: it is the
// only shape that exercises both halves of wantDescription above.
var fixtureDefinition = domain.PlaylistDefinition{
	Mode:     domain.PlaylistModeMinPlays,
	Sort:     domain.SortByPlays,
	Limit:    50,
	MinPlays: 3,
	From:     time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
	To:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
}

// fixtureBuiltAt is when the playlist was last built, and so the date the
// description carries.
var fixtureBuiltAt = time.Date(2026, time.July, 26, 9, 30, 0, 0, time.UTC)

// --- fakes ------------------------------------------------------------------

// getCall is one lookup the handler made, with the arguments it was scoped by.
type getCall struct {
	userID uuid.UUID
	id     uuid.UUID
}

// renameCall is one local rename the handler recorded.
type renameCall struct {
	userID uuid.UUID
	id     uuid.UUID
	name   string
	// spotifyPuts is how many rename requests Spotify had received by the moment
	// this call was made. It is what pins the ordering: a local write that ran
	// ahead of the remote one carries zero here, and no other assertion in this
	// file could tell the difference.
	spotifyPuts int
}

// fakePlaylists stands in for accounts.Playlists.
//
// Get reproduces the repository's owner predicate rather than ignoring it, so
// "somebody else's playlist" is a state these tests can actually reach.
type fakePlaylists struct {
	row       domain.Playlist
	renameErr error
	// spotifyPuts reports how many renames Spotify has been sent so far.
	spotifyPuts func() int

	gets    []getCall
	renames []renameCall
}

func (f *fakePlaylists) Get(
	_ context.Context, _ store.Querier, userID, id uuid.UUID,
) (domain.Playlist, error) {
	f.gets = append(f.gets, getCall{userID: userID, id: id})
	if userID != f.row.UserID || id != f.row.ID {
		return domain.Playlist{}, fmt.Errorf("%w: no such playlist", domain.ErrNotFound)
	}
	return f.row, nil
}

func (f *fakePlaylists) Rename(
	_ context.Context, _ store.Querier, userID, id uuid.UUID, name string,
) (domain.Playlist, error) {
	call := renameCall{userID: userID, id: id, name: name}
	if f.spotifyPuts != nil {
		call.spotifyPuts = f.spotifyPuts()
	}
	f.renames = append(f.renames, call)
	if f.renameErr != nil {
		return domain.Playlist{}, f.renameErr
	}
	f.row.Name = name
	return f.row, nil
}

func (f *fakePlaylists) Create(
	context.Context, store.Querier, domain.Playlist,
) (domain.Playlist, error) {
	return domain.Playlist{}, errors.New("no rename test creates a playlist")
}

func (f *fakePlaylists) ListForUser(
	context.Context, store.Querier, uuid.UUID,
) ([]domain.Playlist, error) {
	return []domain.Playlist{f.row}, nil
}

func (f *fakePlaylists) RecordBuild(
	context.Context, store.Querier, uuid.UUID, int, time.Time,
) error {
	return errors.New("no rename test rebuilds a playlist")
}

func (f *fakePlaylists) Forget(context.Context, store.Querier, uuid.UUID, uuid.UUID) error {
	return errors.New("no rename test forgets a playlist")
}

func (f *fakePlaylists) SetCover(
	context.Context, store.Querier, uuid.UUID, uuid.UUID, domain.PlaylistCover,
) error {
	return errors.New("no rename test sets a cover")
}

// renameRequest is one PUT /v1/playlists/{id} the handler sent.
type renameRequest struct {
	playlistID  string
	name        string
	description string
}

// renameStub is the fake Spotify the real client is pointed at.
//
// The real *spotify.Client stays in the path on purpose. Three of the four
// outcomes this endpoint exists to tell apart are decided by that client's own
// classification of what came back — a 429 becomes a PausedError, a 4xx an
// APIError, a dropped connection neither — and a hand-written double would let
// the handler keep passing while the classification it reads changed underneath
// it.
type renameStub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []renameRequest
}

// newRenameStub serves PUT /v1/playlists/{id}, recording every request before
// respond decides what to answer. A nil respond accepts the rename.
func newRenameStub(t *testing.T, respond http.HandlerFunc) *renameStub {
	t.Helper()

	stub := &renameStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/playlists/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		stub.mu.Lock()
		stub.requests = append(stub.requests, renameRequest{
			playlistID:  r.PathValue("id"),
			name:        body.Name,
			description: body.Description,
		})
		stub.mu.Unlock()

		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"snapshot_id":"snap"}`)
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// puts is how many renames Spotify has been asked for.
func (s *renameStub) puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// sent returns the requests Spotify received.
func (s *renameStub) sent() []renameRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]renameRequest(nil), s.requests...)
}

// --- scripted refusals ------------------------------------------------------

// spotifyStatus answers with one of Spotify's own error envelopes.
func spotifyStatus(status int, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w,
			fmt.Sprintf(`{"error":{"status":%d,"message":%q}}`, status, message))
	}
}

// spotifyRateLimited answers 429 with a Retry-After, which the client turns into
// a PausedError carrying the instant the pause lifts.
func spotifyRateLimited(seconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

// spotifyDropsTheConnection answers nothing at all: the request arrives and the
// connection dies before a response does. That is the state Encore cannot see
// through, and it is deliberately not a status code — a refusal Spotify sent is
// a different fact from an answer that never came.
func spotifyDropsTheConnection() http.HandlerFunc {
	return func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }
}

// --- clock ------------------------------------------------------------------

// frozenClock keeps the Spotify client's idea of now fixed, so a 429's "until"
// instant is a string a test can assert rather than whatever the wall clock
// happened to say.
type frozenClock struct{ at time.Time }

func (c frozenClock) Now() time.Time { return c.at }

func (c frozenClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// --- log capture ------------------------------------------------------------

// logSink records what the handlers logged.
//
// Enabled deliberately drops anything below Info, which is what a deployment
// does: a record written at Debug is not a trace of anything, because nobody
// has that level turned on. That is the whole point of capturing at all here.
type logSink struct {
	mu      sync.Mutex
	records []slog.Record
}

func (s *logSink) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r.Clone())
	return nil
}

func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }

func (s *logSink) WithGroup(string) slog.Handler { return s }

// find returns the records written under one message.
func (s *logSink) find(message string) []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []slog.Record
	for _, r := range s.records {
		if r.Message == message {
			out = append(out, r)
		}
	}
	return out
}

// attr reads one attribute off a record.
func attr(r slog.Record, key string) (slog.Value, bool) {
	var (
		value slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			value, found = a.Value, true
			return false
		}
		return true
	})
	return value, found
}

// --- harness ----------------------------------------------------------------

// renameHarness is a server whose playlist repository and Spotify client are
// both under the test's control.
type renameHarness struct {
	*testServer
	playlists *fakePlaylists
	stub      *renameStub
	// clock is what the Spotify client reads, so a paused instant is predictable.
	clock frozenClock
	// logs is everything the server wrote at Info or above.
	logs *logSink
}

// newRenameHarness wires a signed-in listener who has granted the playlist
// scope, one stored playlist, and a Spotify that answers as respond says.
func newRenameHarness(t *testing.T, respond http.HandlerFunc) *renameHarness {
	t.Helper()

	ts := newTestServer(t)
	stub := newRenameStub(t, respond)
	clock := frozenClock{at: ts.clock}

	playlists := &fakePlaylists{
		row: domain.Playlist{
			ID:         uuid.New(),
			UserID:     ts.sessions.user.ID,
			Name:       fixtureName,
			SpotifyID:  storedSpotifyID,
			SpotifyURL: "https://open.spotify.com/playlist/" + storedSpotifyID,
			Definition: fixtureDefinition,
			TrackCount: 42,
			BuiltAt:    fixtureBuiltAt,
			// What every playlist made before covers existed reads back as.
			Cover:     domain.PlaylistCover{State: domain.CoverNone},
			CreatedAt: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
		},
		spotifyPuts: stub.puts,
	}

	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "user-access-token",
		RefreshToken:   "user-refresh-token",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		Scopes:         append(config.DefaultScopes(), spotify.ScopePlaylistPrivate),
		SyncState:      domain.SyncStateOK,
	}

	logs := &logSink{}
	ts.Server.log = slog.New(logs)
	ts.Server.playlists = playlists
	ts.Server.spotify = newRenameClient(stub, clock)
	ts.Server.userToken = func(context.Context, uuid.UUID) (string, error) {
		return "user-access-token", nil
	}

	return &renameHarness{
		testServer: ts, playlists: playlists, stub: stub, clock: clock, logs: logs,
	}
}

// newRenameClient points the real Spotify client at the stub.
//
// MaxRetries is zero because these tests assert what the handler does with a
// failure, not how the client reaches one: the backoff schedule is
// internal/spotify's own test's subject, and running it here would only make a
// dropped connection take fifteen seconds to become an assertion.
func newRenameClient(stub *renameStub, clock spotify.Clock) *spotify.Client {
	return spotify.NewClient(config.Spotify{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RateLimit:    1000,
		RateBurst:    100,
		Timeout:      5 * time.Second,
		MaxRetries:   0,
	},
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		spotify.WithHTTPClient(stub.server.Client()),
		spotify.WithBaseURL(stub.server.URL),
		spotify.WithClock(clock),
	)
}

// patch sends a PATCH the way the browser client does, cookies and CSRF header
// and all.
func (h *renameHarness) patch(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	r := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	h.signedIn(r)
	r.Header.Set(CSRFHeaderName, h.sessions.session.CSRFToken)
	return h.do(r)
}

// rename asks for the stored playlist to be renamed.
func (h *renameHarness) rename(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()
	return h.patch(t, "/api/playlists/"+h.playlists.row.ID.String(), map[string]any{"name": name})
}

// messageOf reads the sentence out of an error envelope.
func messageOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not an error envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Message
}

// --- the ordering -----------------------------------------------------------

// TestRenameWritesToSpotifyBeforeEncore pins the ordering that is the whole
// safety story of the first write this project makes to a real account.
//
// Fails when: the local Rename moves above the Spotify call — the fake below
// refuses, and the stored name would then already have changed.
func TestRenameWritesToSpotifyBeforeEncore(t *testing.T) {
	t.Run("a refusal leaves the local row alone", func(t *testing.T) {
		h := newRenameHarness(t, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))

		rec := h.rename(t, "Something else")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("PATCH = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		if len(h.playlists.renames) != 0 {
			t.Fatalf("Encore recorded a rename Spotify refused: %+v", h.playlists.renames)
		}
		if h.playlists.row.Name != fixtureName {
			t.Fatalf("the stored name is now %q, want %q: Encore is claiming a name "+
				"Spotify never accepted", h.playlists.row.Name, fixtureName)
		}
	})

	t.Run("a success is recorded only after Spotify has it", func(t *testing.T) {
		h := newRenameHarness(t, nil)

		rec := h.rename(t, "Something else")
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if len(h.playlists.renames) != 1 {
			t.Fatalf("%d local renames, want 1", len(h.playlists.renames))
		}
		if got := h.playlists.renames[0].spotifyPuts; got != 1 {
			t.Fatalf("the local rename ran after %d Spotify writes, want 1: Encore recorded "+
				"a name before the account that owns it had accepted one", got)
		}
	})
}

// TestRenameRecordsTheNameSpotifyAccepted covers the ordinary outcome, and the
// description that goes out beside the name.
//
// Fails when: the description is dropped from the request, or Describe is fed
// something other than the stored definition and the last build — the literal
// above carries both of this phase's description defects.
func TestRenameRecordsTheNameSpotifyAccepted(t *testing.T) {
	h := newRenameHarness(t, nil)

	rec := h.rename(t, "  Quiet hours  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var out Playlist
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a playlist: %v (%s)", err, rec.Body.String())
	}
	if out.Name != "Quiet hours" {
		t.Errorf("response name = %q, want the trimmed %q", out.Name, "Quiet hours")
	}

	sent := h.stub.sent()
	if len(sent) != 1 {
		t.Fatalf("%d requests reached Spotify, want 1", len(sent))
	}
	if sent[0].name != "Quiet hours" {
		t.Errorf("Spotify was sent the name %q, want %q", sent[0].name, "Quiet hours")
	}
	if sent[0].description != wantDescription {
		t.Errorf("Spotify was sent the description\n  %q\nwant\n  %q",
			sent[0].description, wantDescription)
	}
	if len(h.playlists.renames) != 1 || h.playlists.renames[0].name != "Quiet hours" {
		t.Errorf("local renames = %+v, want one for %q", h.playlists.renames, "Quiet hours")
	}
}

// TestRenameAcceptsAHundredCharactersOfAnyAlphabet keeps the ceiling a count of
// characters rather than of bytes, and keeps the name that reaches somebody's
// account the one they typed.
//
// Fails when: the length check counts len(name) rather than runes, or anything
// on this path truncates the name to a byte length — which for a multi-byte
// name is not a shorter name but a different one.
func TestRenameAcceptsAHundredCharactersOfAnyAlphabet(t *testing.T) {
	h := newRenameHarness(t, nil)

	name := strings.Repeat("é", domain.PlaylistMaxNameLength)
	// The fixture has to be able to show the difference: a name of exactly the
	// ceiling in characters must be over it in bytes, or a byte-counting limit
	// would accept it too and this test would prove nothing.
	if len(name) <= domain.PlaylistMaxNameLength {
		t.Fatalf("the fixture is %d bytes for %d characters, so a byte-counting limit "+
			"would accept it as well", len(name), domain.PlaylistMaxNameLength)
	}

	rec := h.rename(t, name)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	sent := h.stub.sent()
	if len(sent) != 1 || sent[0].name != name {
		t.Fatalf("Spotify was sent %+v, want the name exactly as it was typed", sent)
	}
}

// --- the four outcomes ------------------------------------------------------

// TestRenameKeepsTheOldNameWhenSpotifyRefuses pins both the status and the
// sentence, because the sentence is the deliverable: a listener who is told
// only "forbidden" does not know whether their playlist is now called
// something they did not choose.
//
// Fails when: the "still has the name it had before" clause is dropped, or the
// handler records the rename locally anyway.
func TestRenameKeepsTheOldNameWhenSpotifyRefuses(t *testing.T) {
	h := newRenameHarness(t, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))

	rec := h.rename(t, "Something else")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	const want = "Spotify refused the rename. The permission may have been revoked; granting " +
		"it again from Settings restores it. The playlist still has the name it had before."
	if got := messageOf(t, rec); got != want {
		t.Errorf("message =\n  %q\nwant\n  %q", got, want)
	}
	if len(h.playlists.renames) != 0 {
		t.Errorf("Encore recorded a rename Spotify refused: %+v", h.playlists.renames)
	}
}

// TestRenameSaysItCannotTellWhenSpotifyDoesNotAnswer is the copy defect this
// endpoint exists to avoid.
//
// A transport failure is not a refusal. The request may have reached Spotify
// and the response may have been lost, so "nothing has changed" is a claim
// about somebody else's account that Encore cannot support — and it is the
// sentence somebody will write by accident, because it reads like the safe one.
//
// Fails when: the transport branch is merged into the refusal branch, or its
// message gains the words "has not been renamed" / "nothing has changed".
func TestRenameSaysItCannotTellWhenSpotifyDoesNotAnswer(t *testing.T) {
	// Both failures leave the same thing unknown. A dropped connection may have
	// been dropped after Spotify applied the write, and a 5xx that outlived the
	// retry budget says nothing about how far the request got inside Spotify.
	cases := map[string]http.HandlerFunc{
		"the connection dies":                spotifyDropsTheConnection(),
		"spotify's own server keeps failing": spotifyStatus(http.StatusInternalServerError, "Server error"),
		"spotify is briefly unavailable":     spotifyStatus(http.StatusServiceUnavailable, "Unavailable"),
	}

	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRenameHarness(t, respond)

			rec := h.rename(t, "Something else")
			if rec.Code != http.StatusConflict {
				t.Fatalf("PATCH = %d, want 409 (%s)", rec.Code, rec.Body.String())
			}

			// The request did arrive. That is precisely why nothing here may say the
			// playlist was left alone: what was lost is the answer, not the request.
			if got := h.stub.puts(); got != 1 {
				t.Fatalf("Spotify received %d requests, want 1: the fixture is not "+
					"exercising a lost answer at all", got)
			}

			message := messageOf(t, rec)
			if !strings.Contains(message, "cannot tell whether the rename went through") {
				t.Errorf("message does not say Encore cannot tell: %q", message)
			}
			for _, claim := range []string{
				"nothing has changed",
				"has not been renamed",
				"still has the name it had before",
			} {
				if strings.Contains(strings.ToLower(message), claim) {
					t.Errorf("message claims %q, which Encore has not confirmed: %q",
						claim, message)
				}
			}
			if len(h.playlists.renames) != 0 {
				t.Errorf("Encore recorded a rename it cannot confirm: %+v", h.playlists.renames)
			}
		})
	}
}

// TestRenameDoesNotClaimIgnoranceOfARefusalItRead is the other half of the same
// rule, and the easier half to miss.
//
// "Encore did not get an answer from Spotify" is a positive assertion about
// Encore's own state. For a status Encore read and branched on it is false, in
// exactly the way "nothing has changed" is false for a lost answer — it only
// fails in the flattering direction. It would also send somebody to check a
// playlist Encore already knows was not renamed, and invite a retry that will
// fail identically for ever on a 400.
//
// Fails when: an answered 4xx falls through to the no-answer branch, which is
// what happens the moment this case is deleted.
func TestRenameDoesNotClaimIgnoranceOfARefusalItRead(t *testing.T) {
	const want = "Spotify would not accept the rename and did not say why. The playlist " +
		"still has the name it had before. If it keeps happening, signing in again from " +
		"Settings is the usual fix."

	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newRenameHarness(t, spotifyStatus(status, "Refused"))

			rec := h.rename(t, "Something else")
			if rec.Code != http.StatusConflict {
				t.Fatalf("PATCH = %d, want 409 (%s)", rec.Code, rec.Body.String())
			}
			message := messageOf(t, rec)
			if strings.Contains(message, "did not get an answer") {
				t.Errorf("Encore read a %d and then told the caller it got no answer: %q",
					status, message)
			}
			if message != want {
				t.Errorf("message =\n  %q\nwant\n  %q", message, want)
			}
			if len(h.playlists.renames) != 0 {
				t.Errorf("Encore recorded a rename Spotify refused: %+v", h.playlists.renames)
			}
		})
	}
}

// TestRenameLogsOnlyTheOutcomeNobodyCanReconstruct keeps the one branch that
// admits ignorance from being the one branch that leaves no trace.
//
// The listener is told nobody knows what happened to their playlist. An
// operator asked "why is my playlist called something I did not choose" needs
// to find that Encore tried, which playlist it was, and what came back —
// writeError logs a sub-500 at Debug, and no deployment runs at Debug.
//
// The refusals are deliberately not logged: they are ordinary answers, fully
// described by the sentence the caller already has.
//
// Fails when: the warning is dropped, written below Info, or loses the id.
func TestRenameLogsOnlyTheOutcomeNobodyCanReconstruct(t *testing.T) {
	const message = "could not tell whether a rename reached spotify"

	t.Run("no answer", func(t *testing.T) {
		h := newRenameHarness(t, spotifyDropsTheConnection())
		h.rename(t, "Something else")

		records := h.logs.find(message)
		if len(records) != 1 {
			t.Fatalf("%d records under %q, want 1: the only outcome Encore cannot "+
				"reconstruct afterwards left no trace at Info or above", len(records), message)
		}
		if records[0].Level < slog.LevelWarn {
			t.Errorf("logged at %s, want Warn or above", records[0].Level)
		}
		id, ok := attr(records[0], "playlist")
		if !ok || id.String() != storedSpotifyID {
			t.Errorf("the record names playlist %v, want %q", id, storedSpotifyID)
		}
	})

	t.Run("an answered refusal", func(t *testing.T) {
		h := newRenameHarness(t, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))
		h.rename(t, "Something else")

		if records := h.logs.find(message); len(records) != 0 {
			t.Errorf("%d records under %q for a refusal Spotify explained", len(records), message)
		}
	})
}

// TestRenameReportsASpotifySuccessEncoreCouldNotRecord pins the fourth
// outcome. Both systems are now real and they disagree; saying "the rename
// failed" would send somebody to do it a second time, and saying nothing would
// leave every Encore screen showing a name the playlist no longer has.
//
// Fails when: the store error is returned bare through writeError, which would
// surface a generic internal error naming neither system.
func TestRenameReportsASpotifySuccessEncoreCouldNotRecord(t *testing.T) {
	h := newRenameHarness(t, nil)
	h.playlists.renameErr = errors.New("connection reset by peer")

	rec := h.rename(t, "Something else")

	// Read before the status, because the sentence is the specific defect: a bare
	// return answers 500 with the vague internal message, which names neither
	// system and leaves a listener believing the rename did not happen.
	const want = "Spotify has the new name, but Encore could not record it. " +
		"The playlist itself is correct; reload this page to see the current state."
	got := messageOf(t, rec)
	if got == vagueInternalMessage {
		t.Fatal("the store failure was returned bare: the answer names neither system, " +
			"so nobody can tell that Spotify does have the new name")
	}
	if got != want {
		t.Errorf("message =\n  %q\nwant\n  %q", got, want)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if h.stub.puts() != 1 {
		t.Errorf("Spotify received %d requests, want 1", h.stub.puts())
	}
}

// TestRenameOutcomesDoNotCollapseIntoOneAnother is the assertion the branches
// exist for, and it also covers the two refusals no other test here reaches.
//
// Each row is a different thing being true of somebody's account: Spotify
// refused and the old name stands; the playlist is gone from Spotify
// altogether; Spotify would not take the request at all; Encore does not know
// what happened; Spotify has the new name and Encore does not. Any two of them
// sharing a sentence would send a listener to do the wrong thing about a
// playlist they cannot see from here.
//
// The distinctness check is over the *expected* strings rather than only over
// what came back, so it still bites when somebody merges two branches and
// updates this table to agree with the merge — which is how a collapse
// actually reaches a repository, rather than as a test failure somebody
// ignores.
//
// Fails when: two branches are merged, or one branch's message is reused for
// another.
func TestRenameOutcomesDoNotCollapseIntoOneAnother(t *testing.T) {
	cases := []struct {
		name       string
		respond    http.HandlerFunc
		storeFails bool
		wantStatus int
		want       string
	}{
		{
			name:       "spotify refused",
			respond:    spotifyStatus(http.StatusForbidden, "Insufficient client scope"),
			wantStatus: http.StatusForbidden,
			want: "Spotify refused the rename. The permission may have been revoked; " +
				"granting it again from Settings restores it. The playlist still has the " +
				"name it had before.",
		},
		{
			name:       "spotify no longer has the playlist",
			respond:    spotifyStatus(http.StatusNotFound, "Not found"),
			wantStatus: http.StatusNotFound,
			want: "Spotify no longer has that playlist — it may have been deleted from " +
				"your account. Encore still has the definition, so you can build it again.",
		},
		{
			name:       "spotify is rate limiting",
			respond:    spotifyRateLimited(60),
			wantStatus: http.StatusConflict,
			want: "Spotify is rate limiting this instance until 2026-07-26T12:01:00Z, so it " +
				"would not accept the rename. Your listening data is unaffected and the " +
				"playlist still has the name it had before; try again after that.",
		},
		{
			name:       "spotify refused without saying why",
			respond:    spotifyStatus(http.StatusBadRequest, "Bad request"),
			wantStatus: http.StatusConflict,
			want: "Spotify would not accept the rename and did not say why. The playlist " +
				"still has the name it had before. If it keeps happening, signing in again " +
				"from Settings is the usual fix.",
		},
		{
			name:       "spotify did not answer",
			respond:    spotifyDropsTheConnection(),
			wantStatus: http.StatusConflict,
			want: "Encore did not get an answer from Spotify, so it cannot tell whether the " +
				"rename went through. Open the playlist in Spotify to check — renaming it " +
				"again is safe either way.",
		},
		{
			name:       "spotify accepted and encore could not record it",
			storeFails: true,
			wantStatus: http.StatusConflict,
			want: "Spotify has the new name, but Encore could not record it. The playlist " +
				"itself is correct; reload this page to see the current state.",
		},
	}

	expected := map[string]string{}
	for _, c := range cases {
		if other, clash := expected[c.want]; clash {
			t.Errorf("%q and %q are meant to answer with the same sentence, so a listener "+
				"cannot tell the two apart: %q", other, c.name, c.want)
		}
		expected[c.want] = c.name
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRenameHarness(t, c.respond)
			if c.storeFails {
				h.playlists.renameErr = errors.New("connection reset by peer")
			}

			rec := h.rename(t, "Something else")
			if rec.Code != c.wantStatus {
				t.Fatalf("PATCH = %d, want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			if got := messageOf(t, rec); got != c.want {
				t.Errorf("message =\n  %q\nwant\n  %q", got, c.want)
			}
			// Only the outcome that says Spotify has the new name attempted a local
			// write at all; every other row is Spotify not having accepted one.
			wantRenames := 0
			if c.storeFails {
				wantRenames = 1
			}
			if len(h.playlists.renames) != wantRenames {
				t.Errorf("%d local renames, want %d: %+v",
					len(h.playlists.renames), wantRenames, h.playlists.renames)
			}
		})
	}
}

// --- what a body may not do -------------------------------------------------

// TestRenameNeverTakesTheSpotifyIdFromTheRequest pins that a body cannot widen
// what this writes to.
//
// Fails when: the handler reads a spotifyId from the request body, or resolves
// the playlist by anything other than Get(ctx, q, user.ID, id).
func TestRenameNeverTakesTheSpotifyIdFromTheRequest(t *testing.T) {
	h := newRenameHarness(t, nil)
	stored := h.playlists.row

	rec := h.patch(t, "/api/playlists/"+stored.ID.String(), map[string]any{
		"name":       "Something else",
		"spotifyId":  bodySpotifyID,
		"spotifyUrl": "https://open.spotify.com/playlist/" + bodySpotifyID,
		"id":         uuid.New().String(),
		"userId":     uuid.New().String(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	sent := h.stub.sent()
	if len(sent) != 1 {
		t.Fatalf("%d requests reached Spotify, want 1", len(sent))
	}
	if sent[0].playlistID != storedSpotifyID {
		t.Fatalf("Spotify was asked to rename %q, want the stored %q: the body chose the "+
			"playlist", sent[0].playlistID, storedSpotifyID)
	}

	// And the row itself was resolved by the path id, scoped to the caller.
	want := []getCall{{userID: h.sessions.user.ID, id: stored.ID}}
	if len(h.playlists.gets) != 1 || h.playlists.gets[0] != want[0] {
		t.Fatalf("the playlist was looked up as %+v, want %+v", h.playlists.gets, want)
	}
	if len(h.playlists.renames) != 1 ||
		h.playlists.renames[0].userID != h.sessions.user.ID ||
		h.playlists.renames[0].id != stored.ID {
		t.Fatalf("the local rename was scoped as %+v, want the caller and the path id",
			h.playlists.renames)
	}
}

// TestRenameHidesWhetherSomebodyElsesPlaylistExists keeps an id from being
// probed for existence.
//
// A 404 that reads differently for "no such playlist" and "not yours" is an
// oracle: an id is 22 characters of somebody else's library, and answering the
// two apart tells a caller which of them are real.
//
// Fails when: the not-found for a foreign row grows its own message or status,
// or the Spotify call moves above the lookup — the stub would then be asked to
// rename a playlist the caller does not own.
func TestRenameHidesWhetherSomebodyElsesPlaylistExists(t *testing.T) {
	foreign := newRenameHarness(t, nil)
	foreign.playlists.row.UserID = uuid.New() // the row exists; it is not the caller's
	foreignRec := foreign.patch(t,
		"/api/playlists/"+foreign.playlists.row.ID.String(),
		map[string]any{"name": "Something else"})

	absent := newRenameHarness(t, nil)
	absentRec := absent.patch(t,
		"/api/playlists/"+uuid.New().String(),
		map[string]any{"name": "Something else"})

	if foreignRec.Code != http.StatusNotFound || absentRec.Code != http.StatusNotFound {
		t.Fatalf("statuses were %d (foreign) and %d (absent), want 404 for both",
			foreignRec.Code, absentRec.Code)
	}
	if foreignRec.Body.String() != absentRec.Body.String() {
		t.Fatalf("somebody else's playlist answers\n  %s\nand a missing one answers\n  %s\n"+
			"which tells a caller which ids are real",
			foreignRec.Body.String(), absentRec.Body.String())
	}
	if foreign.stub.puts() != 0 {
		t.Error("Encore asked Spotify to rename a playlist the caller does not own")
	}
	if absent.stub.puts() != 0 {
		t.Error("Encore called Spotify for a playlist it does not have")
	}
}

// TestRenameAsksForThePermissionRatherThanBlamingSpotify keeps the two 403s
// apart.
//
// A scope the listener never granted is not Spotify refusing: nothing was
// asked of Spotify at all. The fix is a consent journey rather than a retry,
// and the account must not be parked as needing reauthorisation over a scope
// nobody ever gave.
//
// Fails when: the missing scope is discovered by calling Spotify and reading
// the 403, or the two 403s are given one message.
func TestRenameAsksForThePermissionRatherThanBlamingSpotify(t *testing.T) {
	h := newRenameHarness(t, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))
	h.credentials.creds.Scopes = config.DefaultScopes() // read-only, as a sign-in grant is

	rec := h.rename(t, "Something else")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}

	const want = "Encore needs permission to create and change playlists on your Spotify " +
		"account. Grant it from Settings — nothing else changes, and you can revoke it in Spotify."
	got := messageOf(t, rec)
	if got != want {
		t.Errorf("message =\n  %q\nwant\n  %q", got, want)
	}
	if strings.Contains(got, "Spotify refused") {
		t.Error("a scope the listener never granted is reported as Spotify refusing")
	}
	if h.stub.puts() != 0 {
		t.Error("Encore asked Spotify to rename a playlist without the scope to do it")
	}
	if len(h.playlists.renames) != 0 {
		t.Errorf("Encore recorded a rename it never attempted: %+v", h.playlists.renames)
	}
}

// TestRenameRefusesWhatItCannotSend covers the requests that never reach
// Spotify at all.
//
// The absent name and the empty one are deliberately different answers: the
// first is a malformed call and the second is somebody trying to clear the
// name, and a client that cannot tell them apart cannot say anything useful
// about either.
//
// Fails when: Name stops being a pointer (absent and empty collapse into one
// message), the name is validated after the Spotify call, or the trim is
// dropped.
func TestRenameRefusesWhatItCannotSend(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    map[string]any
		wantMsg string
	}{
		{
			name:    "not a playlist id",
			path:    "/api/playlists/not-a-uuid",
			body:    map[string]any{"name": "Something else"},
			wantMsg: "That is not a valid playlist id.",
		},
		{
			name:    "no name at all",
			body:    map[string]any{"sort": "time"},
			wantMsg: `"name" is required.`,
		},
		{
			name:    "an empty name",
			body:    map[string]any{"name": ""},
			wantMsg: "A playlist needs a name.",
		},
		{
			name:    "a name of only spaces",
			body:    map[string]any{"name": "     "},
			wantMsg: "A playlist needs a name.",
		},
		{
			name:    "a name past the ceiling",
			body:    map[string]any{"name": strings.Repeat("é", domain.PlaylistMaxNameLength+1)},
			wantMsg: "The name may be at most 100 characters.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRenameHarness(t, nil)
			path := c.path
			if path == "" {
				path = "/api/playlists/" + h.playlists.row.ID.String()
			}

			rec := h.patch(t, path, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PATCH = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := messageOf(t, rec); got != c.wantMsg {
				t.Errorf("message = %q, want %q", got, c.wantMsg)
			}
			if h.stub.puts() != 0 {
				t.Error("a request Encore refused still reached Spotify")
			}
			if len(h.playlists.renames) != 0 {
				t.Errorf("a request Encore refused was recorded: %+v", h.playlists.renames)
			}
		})
	}
}

// TestPlaylistCoverIsRenderedFromTheStoredState pins the block every playlist
// response now carries.
//
// Kind is derived rather than stored, which is the whole point of deriving it:
// "mosaic" and "pattern" cannot disagree with the tile count they were built
// from. And it is empty unless a cover is actually ready, so a client cannot
// render a kind for a playlist that has no picture.
//
// Fails when: Kind is set for a state other than "ready", the mosaic and
// pattern arms are swapped, or Total stops being the constant denominator.
func TestPlaylistCoverIsRenderedFromTheStoredState(t *testing.T) {
	at := time.Date(2026, time.July, 20, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		cover domain.PlaylistCover
		want  PlaylistCover
	}{
		{
			name:  "never attempted",
			cover: domain.PlaylistCover{State: domain.CoverNone},
			want:  PlaylistCover{State: "none", Total: domain.CoverTileTotal},
		},
		{
			name:  "a full mosaic",
			cover: domain.PlaylistCover{State: domain.CoverReady, Tiles: 4, At: at},
			want: PlaylistCover{
				State: "ready", Kind: "mosaic", Covered: 4,
				Total: domain.CoverTileTotal, At: &at,
			},
		},
		{
			name:  "a partial mosaic is still a mosaic",
			cover: domain.PlaylistCover{State: domain.CoverReady, Tiles: 2, At: at},
			want: PlaylistCover{
				State: "ready", Kind: "mosaic", Covered: 2,
				Total: domain.CoverTileTotal, At: &at,
			},
		},
		{
			name:  "no artwork at all is the generated pattern",
			cover: domain.PlaylistCover{State: domain.CoverReady, At: at},
			want: PlaylistCover{
				State: "ready", Kind: "pattern", Total: domain.CoverTileTotal, At: &at,
			},
		},
		{
			name: "a failure carries its reason and no kind",
			cover: domain.PlaylistCover{
				State: domain.CoverFailed, Error: "Spotify would not take the image.", At: at,
			},
			want: PlaylistCover{
				State: "failed", Reason: "Spotify would not take the image.",
				Total: domain.CoverTileTotal, At: &at,
			},
		},
		{
			name:  "an ungranted scope is not a failure",
			cover: domain.PlaylistCover{State: domain.CoverUnauthorised, At: at},
			want: PlaylistCover{
				State: "unauthorised", Total: domain.CoverTileTotal, At: &at,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toPlaylist(domain.Playlist{ID: uuid.New(), Cover: c.cover}).Cover
			if got.At == nil || c.want.At == nil {
				if (got.At == nil) != (c.want.At == nil) {
					t.Fatalf("cover.at = %v, want %v", got.At, c.want.At)
				}
			} else if !got.At.Equal(*c.want.At) {
				t.Fatalf("cover.at = %s, want %s", got.At, c.want.At)
			}
			got.At, c.want.At = nil, nil
			if got != c.want {
				t.Errorf("cover = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestRenameNeedsASession keeps the write inside the same session and CSRF
// envelope as everything else under /api.
func TestRenameNeedsASession(t *testing.T) {
	h := newRenameHarness(t, nil)
	path := "/api/playlists/" + h.playlists.row.ID.String()

	anonymous := httptest.NewRequest(http.MethodPatch, path,
		strings.NewReader(`{"name":"Something else"}`))
	anonymous.Header.Set("Content-Type", "application/json")
	if rec := h.do(anonymous); rec.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous rename returned %d, want 401", rec.Code)
	}

	noToken := h.signedIn(httptest.NewRequest(http.MethodPatch, path,
		strings.NewReader(`{"name":"Something else"}`)))
	noToken.Header.Set("Content-Type", "application/json")
	if rec := h.do(noToken); rec.Code != http.StatusForbidden {
		t.Errorf("a rename without a CSRF token returned %d, want 403", rec.Code)
	}
	if h.stub.puts() != 0 {
		t.Error("a request that never got past the middleware still reached Spotify")
	}
}

// --- the cover ---------------------------------------------------------------
//
// coverFor is exercised directly here, never through a full POST /api/playlists
// round trip: it needs no database of its own (its one call into
// internal/stats short-circuits on an empty track list without touching a
// querier), and this package's own tests carry none — see this file's package
// doc. The full round trip, a create that returns 201 while the cover fails
// completely, is proven end to end in test/e2e
// (TestPlaylistCoverFailureDoesNotFailTheCreate), which is where a real track
// selection lives.

// fakeCoverFetcher stands in for the real *playlistcover.Fetcher. Every tile
// comes back nil -- the shape a CDN that answered nothing produces -- which is
// enough to exercise coverFor's own handling of the outcome without a network.
type fakeCoverFetcher struct{ calls int }

func (f *fakeCoverFetcher) Fetch(
	context.Context, [playlistcover.Tiles]string,
) [playlistcover.Tiles]image.Image {
	f.calls++
	var out [playlistcover.Tiles]image.Image
	return out
}

// coverImagesRequest is one PUT /v1/playlists/{id}/images the handler sent.
type coverImagesRequest struct {
	playlistID string
	bodyLen    int
}

// coverUploadStub is the fake Spotify a cover test points the real client at,
// on the same terms renameStub does for a rename: the real *spotify.Client
// stays in the path, so a 403 here is classified by the client's own rules
// rather than by a hand-written double that could drift from them.
type coverUploadStub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []coverImagesRequest
}

// newCoverUploadStub serves PUT /v1/playlists/{id}/images. A nil respond
// accepts the cover; anything else answers with whatever respond writes.
func newCoverUploadStub(t *testing.T, respond http.HandlerFunc) *coverUploadStub {
	t.Helper()

	stub := &coverUploadStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/playlists/{id}/images", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		stub.mu.Lock()
		stub.requests = append(stub.requests, coverImagesRequest{
			playlistID: r.PathValue("id"), bodyLen: len(body),
		})
		stub.mu.Unlock()

		if respond != nil {
			respond(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// puts is how many cover uploads Spotify has been asked for.
func (s *coverUploadStub) puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// newCoverClient points the real Spotify client at the stub, on the same
// terms newRenameClient does and for the same reason: three of the outcomes
// this path cares about are the client's own classification of what came
// back, not something a hand double can be trusted to reproduce.
func newCoverClient(stub *coverUploadStub, clock spotify.Clock) *spotify.Client {
	return spotify.NewClient(config.Spotify{
		ClientID: "client-id", ClientSecret: "client-secret",
		RateLimit: 1000, RateBurst: 100, Timeout: 5 * time.Second, MaxRetries: 0,
	},
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		spotify.WithHTTPClient(stub.server.Client()),
		spotify.WithBaseURL(stub.server.URL),
		spotify.WithClock(clock),
	)
}

// coverFixture wires a *Server for calling coverFor directly: a signed-in
// listener, a real Spotify client pointed at coverUploadStub, and a fake
// fetcher. trackIDs is deliberately left empty by every test that uses this,
// which is what keeps the whole thing off a database (see the section doc
// above).
type coverFixture struct {
	*testServer
	fetcher *fakeCoverFetcher
	stub    *coverUploadStub
}

func newCoverFixture(t *testing.T, scopes []string, respond http.HandlerFunc) *coverFixture {
	t.Helper()

	ts := newTestServer(t)
	stub := newCoverUploadStub(t, respond)
	clock := frozenClock{at: ts.clock}
	fetcher := &fakeCoverFetcher{}

	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "user-access-token",
		RefreshToken:   "user-refresh-token",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		Scopes:         scopes,
		SyncState:      domain.SyncStateOK,
	}
	ts.Server.spotify = newCoverClient(stub, clock)
	ts.Server.covers = fetcher
	ts.Server.userToken = func(context.Context, uuid.UUID) (string, error) {
		return "user-access-token", nil
	}

	return &coverFixture{testServer: ts, fetcher: fetcher, stub: stub}
}

// playlist is the fixture playlist coverFor is asked to cover.
func (f *coverFixture) playlist() domain.Playlist {
	return domain.Playlist{
		ID: uuid.New(), UserID: f.sessions.user.ID,
		Name: "Heavy rotation", SpotifyID: storedSpotifyID,
		Definition: fixtureDefinition,
	}
}

// TestCoverNoneCarriesNoTimestamp pins the domain invariant coverFor's two
// "not configured" short circuits must respect: At has to stay the zero value
// whenever State is CoverNone, because domain.PlaylistCover's own doc says so
// ("Zero while State is CoverNone") and migration 00016's
// playlists_cover_at_matches_state CHECK enforces it in the database --
// (cover_state = 'none') = (cover_at IS NULL). A caller that got this wrong
// would never see the failure: recordCover only logs a rejected write, it
// does not surface one, so the row silently keeps whatever it held before.
//
// Fails when: either short circuit stamps At with s.now() instead of leaving
// it at time.Time's zero value -- which is exactly the shape review found in
// the first version of this code, confirmed end to end in
// test/integration/playlistcover_test.go's
// TestSetCoverRejectsANoneStateWithATimestamp.
func TestCoverNoneCarriesNoTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		covers coverFetcher
		token  func(context.Context, uuid.UUID) (string, error)
	}{
		{
			name:   "no fetcher configured",
			covers: nil,
			token:  func(context.Context, uuid.UUID) (string, error) { return "tok", nil },
		},
		{
			name:   "no user-token function configured",
			covers: &fakeCoverFetcher{},
			token:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.Server.covers = tc.covers
			ts.Server.userToken = tc.token

			got := ts.Server.coverFor(context.Background(), ts.sessions.user, domain.Playlist{
				ID: uuid.New(), UserID: ts.sessions.user.ID, SpotifyID: storedSpotifyID,
			}, nil)

			if got.State != domain.CoverNone {
				t.Fatalf("state = %q, want %q", got.State, domain.CoverNone)
			}
			if !got.At.IsZero() {
				t.Fatalf("At = %s, want the zero value: a non-zero At beside CoverNone "+
					"is a row migration 00016's CHECK constraint refuses to let SetCover "+
					"write", got.At)
			}
		})
	}
}

// TestAMissingImageScopeIsUnauthorisedNotFailed pins the two states apart, and
// pins that no request is spent discovering it.
//
// Fails when: the scope check is dropped and the 403 is classified as
// CoverFailed — the row then offers a retry button for a state a retry cannot
// fix, and one Spotify request is spent per attempt to be told the same thing.
func TestAMissingImageScopeIsUnauthorisedNotFailed(t *testing.T) {
	// The stub would answer exactly what a real Spotify does for a token
	// lacking the scope, *if* it were ever asked. The scope check inside
	// coverFor must make asking unnecessary: the answer is already on the
	// credential row.
	scopes := append(config.DefaultScopes(), spotify.ScopePlaylistPrivate) // no ScopeImageUpload
	f := newCoverFixture(t, scopes, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))

	got := f.Server.coverFor(context.Background(), f.sessions.user, f.playlist(), nil)

	if got.State != domain.CoverUnauthorised {
		t.Fatalf("state = %q, want %q", got.State, domain.CoverUnauthorised)
	}
	if got.Error != "" {
		t.Errorf("an unauthorised cover carries a reason %q, want none: "+
			"a reason is CoverFailed's field, and offering one here would dress up "+
			"a permission prompt as a retry button", got.Error)
	}
	if f.fetcher.calls != 0 {
		t.Errorf("the fetcher was asked for art %d times, want 0: there is nothing to "+
			"upload a cover for without the permission to upload one", f.fetcher.calls)
	}
	if n := f.stub.puts(); n != 0 {
		t.Errorf("spotify received %d requests, want 0: the missing scope is known "+
			"locally, so no request should be spent learning it a second time", n)
	}
}

// TestAnImageScope403NeverParksTheAccount pins the rule at
// internal/sync/account.go:296 for a write scope.
//
// Fails when: the cover path reaches MarkNeedsReauth, or retries — an account
// whose listening history reads perfectly would stop being ingested because a
// decorative image was refused.
func TestAnImageScope403NeverParksTheAccount(t *testing.T) {
	// The scope IS granted here, unlike the test above: coverFailureReason
	// must read this as the grant having been revoked between the check and
	// the call, not as "never granted".
	scopes := append(config.DefaultScopes(), spotify.ScopePlaylistPrivate, spotify.ScopeImageUpload)
	f := newCoverFixture(t, scopes, spotifyStatus(http.StatusForbidden, "Insufficient client scope"))

	got := f.Server.coverFor(context.Background(), f.sessions.user, f.playlist(), nil)

	if got.State != domain.CoverFailed {
		t.Fatalf("state = %q, want %q", got.State, domain.CoverFailed)
	}
	const want = "Spotify refused the cover. The permission may have been revoked."
	if got.Error != want {
		t.Errorf("error = %q, want %q", got.Error, want)
	}
	if n := f.stub.puts(); n != 1 {
		t.Errorf("spotify received %d requests, want exactly 1: a scope refusal must "+
			"not be retried, which would spend quota to be told the same thing again", n)
	}
	if f.credentials.upserts != 0 {
		t.Errorf("the credential row was written %d times; a 403 on an optional scope "+
			"must never touch it the way markNeedsReauth would — that would stop "+
			"ingesting an account whose listening history still reads perfectly",
			f.credentials.upserts)
	}
}

// TestAFailingCoverDoesNotFailTheCreate is the property the whole feature
// rests on: a playlist that exists with a grey cover is a far better outcome
// than a create that reports failure because a CDN was slow.
//
// Fails when: coverFor is given an error return and a caller propagates it, or
// the create's SetCover call is allowed to fail the request.
//
// The two halves are proven separately. coverFor's signature is the first:
// it returns a domain.PlaylistCover and nothing else, so there is no error for
// handleCreatePlaylist or handleRebuildPlaylist to check, let alone
// propagate — a property the compiler enforces, and this subtest exercises it
// under total failure (no art, and Spotify refuses the upload) to show the
// result is a reportable state rather than a panic. recordCover is the
// second: it wraps the one call that writes the outcome to storage and, like
// coverFor, returns nothing, so a repository failure has no path back to the
// request that triggered it.
//
// The full round trip — POST /api/playlists answering 201 with the right
// track count while the cover fails completely — needs a real track
// selection, which needs a real database; this package's tests carry
// neither (see this file's package doc). That half is proven end to end in
// test/e2e (TestPlaylistCoverFailureDoesNotFailTheCreate).
func TestAFailingCoverDoesNotFailTheCreate(t *testing.T) {
	t.Run("total failure still returns a state, not a panic", func(t *testing.T) {
		scopes := append(config.DefaultScopes(), spotify.ScopePlaylistPrivate, spotify.ScopeImageUpload)
		f := newCoverFixture(t, scopes, spotifyStatus(http.StatusInternalServerError, "Server error"))

		got := f.Server.coverFor(context.Background(), f.sessions.user, f.playlist(), nil)

		if got.State != domain.CoverFailed {
			t.Fatalf("state = %q, want %q: a total failure must still be a reportable "+
				"state", got.State, domain.CoverFailed)
		}
		if got.Error == "" {
			t.Error("a failed cover carries no reason for the listener to read")
		}
	})

	t.Run("recordCover swallows a repository failure", func(t *testing.T) {
		ts := newTestServer(t)
		logs := &logSink{}
		ts.Server.log = slog.New(logs)
		ts.Server.playlists = &fakePlaylists{} // SetCover errors unconditionally

		// logging.FromContext reads the logger middleware attaches to a real
		// request's context; recordCover is called directly here, with no
		// request behind it, so the context has to carry it the same way.
		ctx := logging.WithLogger(context.Background(), ts.Server.log)

		// recordCover returns nothing at all: there is no channel back to a
		// caller through which a repository failure here could fail the
		// request that produced the cover it is recording.
		ts.Server.recordCover(ctx, uuid.New(), uuid.New(),
			domain.PlaylistCover{State: domain.CoverReady, Tiles: 2, At: ts.clock})

		if records := logs.find("could not record a playlist cover state"); len(records) != 1 {
			t.Fatalf("%d records logging the swallowed failure, want 1", len(records))
		}
	})
}
