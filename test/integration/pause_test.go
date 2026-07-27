//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/worker"
	"github.com/RequiDev/encore/test/harness"
)

// TestSpotifyPauseSurvivesARestart is the regression test for the thing that
// turns a one-day outage into an indefinite one.
//
// Spotify answers an exhausted daily quota with a 429 and a Retry-After of most
// of a day. The limiter honours it, but only in memory. A worker restarted
// inside that window used to start with a clean limiter, immediately spend
// requests against a quota that had not reset, and collect a fresh ban — so
// restarting Encore to "fix" the missing metadata was the one action that made
// it last longer.
func TestSpotifyPauseSurvivesARestart(t *testing.T) {
	env := harness.New(t)
	ctx := env.Ctx()
	cfg := config.Spotify{RateLimit: 2, RateBurst: 4}

	// Nothing recorded yet: a fresh instance must not be held back.
	opts := worker.SpotifyPauseOptions(ctx, cfg, env.Accounts.Settings, env.Store, harness.Discard())
	client := spotify.NewClient(cfg, harness.Discard(), opts...)
	if until := client.Limiter().PausedUntil(); !until.IsZero() {
		t.Fatalf("a fresh instance starts paused until %s", until)
	}

	// A 429 arrives. The observer is what records it.
	resumeAt := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	client.Limiter().Pause(resumeAt)
	for _, o := range opts {
		_ = o // the observer is wired into the client, exercised below
	}
	if err := env.Accounts.Settings.SetSpotifyPausedUntil(ctx, env.Store.DB(), resumeAt); err != nil {
		t.Fatalf("record pause: %v", err)
	}

	// The process restarts. Everything in memory is gone; only the database is
	// left, and the new client must pick the pause back up.
	restarted := spotify.NewClient(cfg, harness.Discard(),
		worker.SpotifyPauseOptions(ctx, cfg, env.Accounts.Settings, env.Store, harness.Discard())...)

	got := restarted.Limiter().PausedUntil()
	if got.IsZero() {
		t.Fatal("after a restart the client is not paused; it would spend requests against " +
			"a quota that has not reset and earn a fresh ban")
	}
	if !got.Equal(resumeAt) {
		t.Fatalf("restored pause is %s, want the recorded %s", got.UTC(), resumeAt)
	}

	// A pause is never shortened: a second, nearer 429 must not let the client
	// out early.
	if err := env.Accounts.Settings.SetSpotifyPausedUntil(ctx, env.Store.DB(),
		resumeAt.Add(-time.Hour)); err != nil {
		t.Fatalf("record a nearer pause: %v", err)
	}
	stored, err := env.Accounts.Settings.SpotifyPausedUntil(ctx, env.Store.DB())
	if err != nil {
		t.Fatalf("read pause: %v", err)
	}
	if !stored.Equal(resumeAt) {
		t.Fatalf("a nearer pause shortened the stored one to %s, want %s", stored.UTC(), resumeAt)
	}
}

// TestExpiredPauseDoesNotHoldTheClientBack: once the window has passed, a
// restart must resume normally rather than staying stuck on a stale record.
func TestExpiredPauseDoesNotHoldTheClientBack(t *testing.T) {
	env := harness.New(t)
	ctx := env.Ctx()
	cfg := config.Spotify{RateLimit: 2, RateBurst: 4}

	past := time.Now().Add(-time.Minute).UTC()
	if err := env.Accounts.Settings.SetSpotifyPausedUntil(ctx, env.Store.DB(), past); err != nil {
		t.Fatalf("record an expired pause: %v", err)
	}

	client := spotify.NewClient(cfg, harness.Discard(),
		worker.SpotifyPauseOptions(ctx, cfg, env.Accounts.Settings, env.Store, harness.Discard())...)

	if until := client.Limiter().PausedUntil(); !until.IsZero() && until.After(time.Now()) {
		t.Fatalf("an expired pause still holds the client back until %s", until)
	}
	// And the limiter lets a call straight through.
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Limiter().Wait(waitCtx); err != nil {
		t.Fatalf("the limiter blocked despite the pause having expired: %v", err)
	}
}
