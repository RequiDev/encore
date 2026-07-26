package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/requi/encore/internal/domain"
)

// refusingQuerier fails the test if a statement reaches it. The validation tests
// use it to prove that bad input is rejected before a connection is spent, which
// is the property that keeps a typo'd timezone from becoming a round trip.
type refusingQuerier struct{ t *testing.T }

func (q refusingQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.t.Helper()
	q.t.Fatalf("unexpected Exec: %s", sql)
	return pgconn.CommandTag{}, nil
}

func (q refusingQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.t.Helper()
	q.t.Fatalf("unexpected Query: %s", sql)
	return nil, nil
}

func (q refusingQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.t.Helper()
	q.t.Fatalf("unexpected QueryRow: %s", sql)
	return nil
}

func columnCount(list string) int { return len(strings.Split(list, ",")) }

// TestColumnListsMatchScanDestinations guards the one invariant a column list and
// its scan helper share: a column added to one and forgotten in the other is a
// runtime scan error that no compiler catches.
func TestColumnListsMatchScanDestinations(t *testing.T) {
	var (
		u    domain.User
		role string
		s    domain.Session
		ip   *string
	)
	cases := []struct {
		name  string
		lists []string
		dest  int
	}{
		{"users", []string{userColumns, userColumnsU}, len(userDest(&u, &role))},
		{"sessions", []string{sessionColumns, sessionColumnsS}, len(sessionDest(&s, &ip))},
	}
	for _, c := range cases {
		for _, list := range c.lists {
			if got := columnCount(list); got != c.dest {
				t.Errorf("%s: column list has %d columns, scan has %d destinations", c.name, got, c.dest)
			}
		}
	}
	if columnCount(credentialColumns) != 10 {
		t.Errorf("credential column list has %d columns, want 10", columnCount(credentialColumns))
	}
}

func TestClampPage(t *testing.T) {
	cases := []struct {
		name                  string
		limit, offset         int
		wantLimit, wantOffset int
	}{
		{"zero limit takes the default", 0, 0, defaultPageSize, 0},
		{"negative limit takes the default", -5, 10, defaultPageSize, 10},
		{"limit is capped", maxPageSize + 1, 0, maxPageSize, 0},
		{"negative offset starts at the beginning", 10, -1, 10, 0},
		{"valid arguments pass through", 25, 75, 25, 75},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limit, offset := clampPage(c.limit, c.offset)
			if limit != c.wantLimit || offset != c.wantOffset {
				t.Fatalf("clampPage(%d, %d) = (%d, %d), want (%d, %d)",
					c.limit, c.offset, limit, offset, c.wantLimit, c.wantOffset)
			}
		})
	}
	if got := clampLimit(0); got != defaultPageSize {
		t.Fatalf("clampLimit(0) = %d, want %d", got, defaultPageSize)
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate did not leave a short string alone: %q", got)
	}
	if got := truncate("abcdefgh", 4); got != "abcd..." {
		t.Fatalf("truncate(%q, 4) = %q", "abcdefgh", got)
	}
	// Cutting mid-rune would produce bytes Postgres rejects for a text column.
	got := truncate("aé😀b", 3)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != "aé..." {
		t.Fatalf("truncate cut on the wrong boundary: %q", got)
	}
}

// timeZero is a stand-in for arguments the validation tests never reach.
func timeZero() time.Time { return time.Time{} }

func TestDecodeBool(t *testing.T) {
	for _, raw := range []string{"true", "false"} {
		v, err := decodeBool("k", json.RawMessage(raw))
		if err != nil {
			t.Fatalf("decodeBool(%s): %v", raw, err)
		}
		if v != (raw == "true") {
			t.Fatalf("decodeBool(%s) = %v", raw, v)
		}
	}
	if _, err := decodeBool(domain.SettingRegistrationsEnabled, json.RawMessage(`"yes"`)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("decodeBool of a non-boolean returned %v, want ErrValidation", err)
	}
}

func TestUsersValidateBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	q := refusingQuerier{t: t}
	repo := NewUsers(nil)

	if _, err := repo.SetTimezone(ctx, q, uuid.New(), "Mars/Olympus_Mons"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SetTimezone with an unknown zone returned %v, want ErrValidation", err)
	}
	if _, err := repo.SetTimezone(ctx, q, uuid.New(), ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SetTimezone with an empty zone returned %v, want ErrValidation", err)
	}
	if _, err := repo.SetRole(ctx, q, uuid.New(), domain.Role("owner")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SetRole with an unknown role returned %v, want ErrValidation", err)
	}
	if _, _, err := repo.UpsertFromSpotify(ctx, q, SpotifyProfile{}, "UTC", true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("UpsertFromSpotify without an identity returned %v, want ErrValidation", err)
	}
}

func TestCredentialsValidateBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	q := refusingQuerier{t: t}
	repo := NewCredentials(nil)

	cases := []struct {
		name  string
		creds domain.SpotifyCredentials
	}{
		{"no user", domain.SpotifyCredentials{AccessToken: "a", RefreshToken: "r"}},
		{"no access token", domain.SpotifyCredentials{UserID: uuid.New(), RefreshToken: "r"}},
		{"no refresh token", domain.SpotifyCredentials{UserID: uuid.New(), AccessToken: "a"}},
		{"unknown sync state", domain.SpotifyCredentials{
			UserID: uuid.New(), AccessToken: "a", RefreshToken: "r", SyncState: domain.SyncState("stale"),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := repo.Upsert(ctx, q, c.creds); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Upsert returned %v, want ErrValidation", err)
			}
		})
	}

	if err := repo.UpdateTokens(ctx, q, uuid.New(), "", "r", timeZero()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("UpdateTokens without an access token returned %v, want ErrValidation", err)
	}
}

func TestSessionsAndOAuthValidateBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	q := refusingQuerier{t: t}

	sessions := NewSessions(nil)
	if _, err := sessions.Create(ctx, q, uuid.New(), nil, "csrf", timeZero(), "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create without a token hash returned %v, want ErrValidation", err)
	}
	if _, err := sessions.Create(ctx, q, uuid.New(), []byte{1}, "", timeZero(), "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create without a csrf token returned %v, want ErrValidation", err)
	}
	if _, _, err := sessions.GetByTokenHash(ctx, q, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("GetByTokenHash without a token hash returned %v, want ErrValidation", err)
	}

	states := NewOAuthStates(nil)
	if err := states.Create(ctx, q, nil, "verifier", "", nil, timeZero()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create without a state hash returned %v, want ErrValidation", err)
	}
	if err := states.Create(ctx, q, []byte{1}, "", "", nil, timeZero()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create without a verifier returned %v, want ErrValidation", err)
	}
	if _, _, _, err := states.Consume(ctx, q, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Consume without a state hash returned %v, want ErrValidation", err)
	}
}

func TestSettingsValidateBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	q := refusingQuerier{t: t}
	repo := NewSettings(nil)

	if _, err := repo.Get(ctx, q, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Get with an empty key returned %v, want ErrValidation", err)
	}
	if err := repo.Set(ctx, q, "", true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Set with an empty key returned %v, want ErrValidation", err)
	}
	// A channel has no JSON representation, so this must be refused rather than
	// written as a broken document.
	if err := repo.Set(ctx, q, "k", make(chan int)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Set with an unencodable value returned %v, want ErrValidation", err)
	}
}

// TestUpsertUserSQLDecidesRoleInStatement pins the property the sign-in race
// depends on: the founding administrator is chosen by the INSERT itself, never
// by a preceding SELECT.
func TestUpsertUserSQLDecidesRoleInStatement(t *testing.T) {
	if !strings.Contains(upsertUserSQL, "CASE WHEN NOT EXISTS (SELECT 1 FROM users) THEN 'admin' ELSE 'user' END") {
		t.Fatal("the first-user-is-admin decision is no longer inside the INSERT")
	}
	if !strings.Contains(upsertUserSQL, "ON CONFLICT (spotify_user_id) DO NOTHING") {
		t.Fatal("a concurrent sign-in for the same identity is no longer settled by the unique constraint")
	}
	if !strings.Contains(upsertUserSQL, "u.is_active") {
		t.Fatal("the refresh branch no longer excludes deactivated accounts")
	}
}

// TestConsumeOAuthStateIsSingleUse pins that the state is destroyed by the same
// statement that reads it, which is what stops a replay.
func TestConsumeOAuthStateIsSingleUse(t *testing.T) {
	if !strings.HasPrefix(strings.TrimSpace(consumeOAuthStateSQL), "DELETE FROM oauth_states") {
		t.Fatal("consuming an oauth state no longer deletes it")
	}
	if !strings.Contains(consumeOAuthStateSQL, "RETURNING") {
		t.Fatal("consuming an oauth state no longer returns the row it deleted")
	}
}
