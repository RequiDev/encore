//go:build integration

package integration

import (
	"testing"
)

// TestPlaylistContextGroupsByTypeAndID pins the grouping rule: listens are
// bucketed by the (context_type, context_id) pair they were played from, a
// playlist owned by the listener is named from user_playlists, and an album
// context sits in its own group next to it.
//
// From the shared fixture, in played_at order: trk-a x3 then trk-b (2024-01-01),
// trk-c x2 (2024-01-02), trk-a x1 (2024-01-03), trk-d (2024-01-05). The first
// three (trk-a) and the fourth (trk-b) are given playlist context; the fifth and
// sixth (trk-c) are given an album context; the last two are left NULL.
func TestPlaylistContextGroupsByTypeAndID(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            context_type = CASE WHEN o.n IN (1,2,3) THEN 'playlist'
                                 WHEN o.n = 4        THEN 'playlist'
                                 WHEN o.n IN (5,6)    THEN 'album'
                                 ELSE NULL END,
            context_id   = CASE WHEN o.n IN (1,2,3) THEN 'pl-1'
                                 WHEN o.n = 4        THEN 'pl-2'
                                 WHEN o.n IN (5,6)    THEN 'alb-2'
                                 ELSE NULL END
        FROM ordered o
        WHERE l.id = o.id`, f.user.ID)
	f.env.Exec(`INSERT INTO user_playlists (user_id, playlist_id, name, fetched_at) VALUES
        ($1, 'pl-1', 'Road Trip', now()), ($1, 'pl-2', 'Chill', now())`, f.user.ID)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	if len(got.Playlists) != 3 {
		t.Fatalf("got %d playlist context groups, want 3: %+v", len(got.Playlists), got.Playlists)
	}
	by := map[string]struct {
		Type, Name string
		Plays      int64
	}{}
	for _, p := range got.Playlists {
		by[p.ContextID] = struct {
			Type, Name string
			Plays      int64
		}{p.ContextType, p.Name, p.Plays}
	}
	if e := by["pl-1"]; e.Type != "playlist" || e.Name != "Road Trip" || e.Plays != 3 {
		t.Errorf("pl-1 = %+v, want playlist/Road Trip/3", e)
	}
	if e := by["pl-2"]; e.Type != "playlist" || e.Name != "Chill" || e.Plays != 1 {
		t.Errorf("pl-2 = %+v, want playlist/Chill/1", e)
	}
	if e := by["alb-2"]; e.Type != "album" || e.Name != "" || e.Plays != 2 {
		t.Errorf("alb-2 = %+v, want album/\"\"/2", e)
	}
	if got.PlaylistCoverage.Covered != 6 || got.PlaylistCoverage.Total != 8 {
		t.Errorf("coverage = %+v, want 6/8", got.PlaylistCoverage)
	}
}

// TestPlaylistContextCoverageIsPerColumn is the rule the whole statistic rests
// on. Five rows are marked source = 0 (live sync, the only source that can ever
// carry context), but only three of those five actually have context_type set —
// the other two represent a recently-played entry Spotify reported with no
// context attached. A denominator keyed on source = 0 would read 5; the correct,
// per-column denominator reads 3.
func TestPlaylistContextCoverageIsPerColumn(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            source       = CASE WHEN o.n <= 5 THEN 0 ELSE l.source END,
            context_type = CASE WHEN o.n IN (1,2,3) THEN 'playlist' ELSE NULL END,
            context_id   = CASE WHEN o.n IN (1,2,3) THEN 'pl-1' ELSE NULL END
        FROM ordered o
        WHERE l.id = o.id`, f.user.ID)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	if got.PlaylistCoverage.Total != 8 {
		t.Errorf("coverage total = %d, want 8", got.PlaylistCoverage.Total)
	}
	if got.PlaylistCoverage.Covered != 3 {
		t.Errorf("coverage covered = %d, want 3 — the denominator was keyed on source = 0 (5), not on context_type IS NOT NULL (3)", got.PlaylistCoverage.Covered)
	}
}

// TestPlaylistContextUnknownPlaylistSurvivesWithEmptyName is the most important
// test of this file. A playlist context whose id names nothing in
// user_playlists — deleted since, or never the listener's own — must still be
// counted, with an empty name, rather than silently dropped from the total.
func TestPlaylistContextUnknownPlaylistSurvivesWithEmptyName(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET context_type = 'playlist', context_id = 'ghost-playlist'
        FROM ordered o
        WHERE l.id = o.id AND o.n = 1`, f.user.ID)
	// Deliberately no row inserted into user_playlists for 'ghost-playlist'.

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	if len(got.Playlists) != 1 {
		t.Fatalf("got %d playlist context groups, want 1: %+v", len(got.Playlists), got.Playlists)
	}
	e := got.Playlists[0]
	if e.ContextType != "playlist" || e.ContextID != "ghost-playlist" || e.Plays != 1 {
		t.Errorf("entry = %+v, want playlist/ghost-playlist/1", e)
	}
	if e.Name != "" {
		t.Errorf("name = %q, want empty — an id that names nothing must not be dropped nor invented a name", e.Name)
	}
	if got.PlaylistCoverage.Covered != 1 {
		t.Errorf("coverage covered = %d, want 1", got.PlaylistCoverage.Covered)
	}
}

// TestPlaylistContextNonPlaylistTypeAppears proves the statistic is not
// silently playlist-only: album and collection (Liked Songs) contexts are
// counted exactly like a playlist one, and user_playlists never names them.
// The collection row also carries a NULL context_id (Spotify's bare
// "spotify:collection" URI has no id segment at all), which must still group
// and report as an empty string rather than break the query.
func TestPlaylistContextNonPlaylistTypeAppears(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            context_type = CASE WHEN o.n = 1 THEN 'collection' WHEN o.n = 2 THEN 'album' END,
            context_id   = CASE WHEN o.n = 1 THEN NULL ELSE 'alb-1' END
        FROM ordered o
        WHERE l.id = o.id AND o.n IN (1,2)`, f.user.ID)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	if len(got.Playlists) != 2 {
		t.Fatalf("got %d playlist context groups, want 2: %+v", len(got.Playlists), got.Playlists)
	}
	by := map[string]int64{}
	names := map[string]string{}
	for _, p := range got.Playlists {
		by[p.ContextType] = p.Plays
		names[p.ContextType] = p.Name
	}
	if by["collection"] != 1 {
		t.Errorf("collection plays = %d, want 1", by["collection"])
	}
	if names["collection"] != "" {
		t.Errorf("collection name = %q, want empty", names["collection"])
	}
	if by["album"] != 1 {
		t.Errorf("album plays = %d, want 1", by["album"])
	}
	if names["album"] != "" {
		t.Errorf("album name = %q, want empty", names["album"])
	}
}

// TestPlaylistContextRespectsTheBlacklist checks blacklistFilter was actually
// composed in, not merely available. trk-a credits art-x; blacklisting art-x
// removes every trk-a listen from the range entirely, which must remove both
// its playlist-context group and its contribution to the coverage denominator.
func TestPlaylistContextRespectsTheBlacklist(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            context_type = CASE WHEN o.n IN (1,2,3,7) THEN 'playlist' WHEN o.n IN (5,6) THEN 'playlist' END,
            context_id   = CASE WHEN o.n IN (1,2,3,7) THEN 'pl-a' WHEN o.n IN (5,6) THEN 'pl-c' END
        FROM ordered o
        WHERE l.id = o.id AND o.n IN (1,2,3,5,6,7)`, f.user.ID)

	before, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context before blacklist: %v", err)
	}
	if before.PlaylistCoverage.Covered != 6 || before.PlaylistCoverage.Total != 8 {
		t.Fatalf("sanity: coverage before blacklist = %+v, want 6/8", before.PlaylistCoverage)
	}

	f.env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	after, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context after blacklist: %v", err)
	}

	if len(after.Playlists) != 1 || after.Playlists[0].ContextID != "pl-c" || after.Playlists[0].Plays != 2 {
		t.Errorf("after blacklist = %+v, want only pl-c with 2 plays", after.Playlists)
	}
	// trk-a (4 plays, all pl-a) and trk-b (1 play, no context) both credit or are
	// covered by art-x's blacklist and disappear from the range entirely: 8 - 4 - 1 = 3.
	if after.PlaylistCoverage.Total != 3 {
		t.Errorf("total after blacklist = %d, want 3", after.PlaylistCoverage.Total)
	}
	if after.PlaylistCoverage.Covered != 2 {
		t.Errorf("covered after blacklist = %d, want 2", after.PlaylistCoverage.Covered)
	}
}

// TestPlaylistContextWithNoContextIsZeroNotAnError is the state every existing
// instance is in the moment this ships: no row has ever had a context column
// touched, and the statistic must report zero coverage rather than error.
func TestPlaylistContextWithNoContextIsZeroNotAnError(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context with no context columns: %v", err)
	}
	if len(got.Playlists) != 0 {
		t.Errorf("got %d playlist context groups, want 0: %+v", len(got.Playlists), got.Playlists)
	}
	if got.PlaylistCoverage.Covered != 0 {
		t.Errorf("coverage covered = %d, want 0", got.PlaylistCoverage.Covered)
	}
	if got.PlaylistCoverage.Total != 8 {
		t.Errorf("coverage total = %d, want 8 — the total is all in-range listens", got.PlaylistCoverage.Total)
	}
}

// TestPlaylistContextLimitIsHonoured seeds sixty distinct playlist contexts,
// one play each, well past the default page size, and checks the breakdown is
// capped rather than materialising every group unbounded. Zero-padded ids tie
// on play count, so the ascending tie-break makes the top of the list
// deterministic: pl-0001 through pl-0050.
func TestPlaylistContextLimitIsHonoured(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`
        INSERT INTO listens (user_id, played_at, track_id, identity_key, dedupe_key,
                              ms_played, source, context_type, context_id)
        SELECT $1, timestamptz '2024-01-01 12:00:00+00' + (g || ' minutes')::interval, 'trk-a',
               convert_to('idk-many-' || g::text, 'UTF8'),
               convert_to('ddk-many-' || g::text, 'UTF8'),
               1000, 0, 'playlist', 'pl-' || lpad(g::text, 4, '0')
        FROM generate_series(1, 60) AS g`, f.user.ID)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	if len(got.Playlists) != 50 {
		t.Fatalf("got %d playlist context groups, want 50 (the default page size)", len(got.Playlists))
	}
	ids := map[string]bool{}
	for _, p := range got.Playlists {
		ids[p.ContextID] = true
	}
	if !ids["pl-0001"] || !ids["pl-0050"] {
		t.Errorf("expected the lexically-first 50 ids, missing pl-0001 or pl-0050: %+v", got.Playlists)
	}
	if ids["pl-0051"] {
		t.Error("pl-0051 should have been cut off by the limit")
	}
	if got.PlaylistCoverage.Total != 68 || got.PlaylistCoverage.Covered != 60 {
		t.Errorf("coverage = %+v, want 60/68", got.PlaylistCoverage)
	}
}
