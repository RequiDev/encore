//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// The tests for making an export's own names into a usable catalogue.
//
// Both Spotify export formats print the artist and the album of every play and
// identify neither: there is a spotify_track_uri and nothing else. Encore used
// to parse those names and drop them, so a history holding three and a half
// thousand artists produced a catalogue with none — every chart empty, every
// session line reading "Unknown artist" — until enrichment drained. On an
// application whose daily quota is exhausted that is not a wait, it is for ever.

// seedFromExport stages listens the way an extended import does: a real track
// id, and names for the artist and album that no id accompanies.
func seedFromExport(t *testing.T, env *harness.Env, userID any, rows ...[4]string) {
	t.Helper()
	user := env.NewUser(userID.(string))

	seeds := make([]listens.TrackSeed, 0, len(rows))
	batch := make([]listens.StagedListen, 0, len(rows))
	at := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)

	for i, r := range rows {
		trackID, trackName, artistName, albumName := r[0], r[1], r[2], r[3]
		seeds = append(seeds, listens.TrackSeed{
			ID: trackID, Name: trackName, ArtistName: artistName, AlbumName: albumName,
		})
		batch = append(batch, listens.Stage(domain.Listen{
			UserID:     user.ID,
			PlayedAt:   at.Add(time.Duration(i) * time.Hour),
			Precision:  domain.PrecisionSecond,
			Identity:   domain.TrackIdentityFromID(trackID),
			MsPlayed:   200_000,
			Source:     domain.SourceExtended,
			TrackName:  trackName,
			ArtistName: artistName,
			AlbumName:  albumName,
		}, nil))
	}

	ctx, db := env.Ctx(), env.Store.DB()
	if err := env.Listens.EnsureTracks(ctx, db, seeds); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	if err := env.Listens.EnsureLocalCatalogue(ctx, db, seeds); err != nil {
		t.Fatalf("ensure local catalogue: %v", err)
	}
	if _, err := env.Listens.InsertListens(ctx, db, batch, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}
}

// TestAnImportBuildsAnArtistCatalogueWithoutSpotify is the headline behaviour.
func TestAnImportBuildsAnArtistCatalogueWithoutSpotify(t *testing.T) {
	env := harness.New(t)
	seedFromExport(t, env, "exportuser",
		[4]string{"trk0000000000000000001", "Weightless", "Marconi Union", "Ambient Transmissions"},
		[4]string{"trk0000000000000000002", "Teardrop", "Massive Attack", "Mezzanine"},
		[4]string{"trk0000000000000000003", "Angel", "Massive Attack", "Mezzanine"},
	)

	// Two artists, three tracks, one album — all named, no Spotify call made.
	if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE metadata_state = 'local'`); got != 2 {
		t.Fatalf("%d local artists, want 2", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE name <> ''`); got != 2 {
		t.Fatalf("%d named artists, want 2", got)
	}
	// One album: the two Massive Attack tracks share it, and it is keyed on the
	// artist as well as the title so it cannot collide with anybody else's.
	if got := env.ScalarInt(`SELECT count(*) FROM albums WHERE metadata_state = 'local'`); got != 2 {
		t.Fatalf("%d local albums, want 2", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM track_artists`); got != 3 {
		t.Fatalf("%d track-artist links, want 3", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE album_id IS NOT NULL`); got != 3 {
		t.Fatalf("%d tracks carry an album, want 3", got)
	}

	// The ids say plainly where they came from, and could never be mistaken for
	// Spotify's or sent to it.
	var artistID string
	if err := env.Store.DB().QueryRow(env.Ctx(),
		`SELECT id FROM artists WHERE name = 'Massive Attack'`).Scan(&artistID); err != nil {
		t.Fatalf("read the artist: %v", err)
	}
	if !domain.IsLocalID(artistID) {
		t.Fatalf("artist id %q is not marked local", artistID)
	}
}

// TestReimportingDoesNotDuplicateTheLocalCatalogue: the ids are derived from the
// names, so the second pass lands on the same rows.
func TestReimportingDoesNotDuplicateTheLocalCatalogue(t *testing.T) {
	env := harness.New(t)
	rows := [][4]string{
		{"trk0000000000000000001", "Weightless", "Marconi Union", "Ambient Transmissions"},
		{"trk0000000000000000002", "Teardrop", "Massive Attack", "Mezzanine"},
	}
	seedFromExport(t, env, "firstpass", rows...)
	before := env.ScalarInt(`SELECT count(*) FROM artists`)

	seedFromExport(t, env, "secondpass", rows...)
	if after := env.ScalarInt(`SELECT count(*) FROM artists`); after != before {
		t.Fatalf("a second import took the artist count from %d to %d", before, after)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM track_artists`); got != 2 {
		t.Fatalf("%d track-artist links after re-importing, want 2", got)
	}
}

// TestSpellingVariantsLandOnOneArtist: the id comes from the normalised name, so
// case and spacing differences do not fracture a catalogue.
func TestSpellingVariantsLandOnOneArtist(t *testing.T) {
	env := harness.New(t)
	seedFromExport(t, env, "variants",
		[4]string{"trk0000000000000000001", "One", "Massive Attack", "Mezzanine"},
		[4]string{"trk0000000000000000002", "Two", "massive attack", "Mezzanine"},
		[4]string{"trk0000000000000000003", "Three", "  Massive   Attack  ", "Mezzanine"},
	)
	if got := env.ScalarInt(`SELECT count(*) FROM artists`); got != 1 {
		t.Fatalf("%d artists for three spellings of one name, want 1", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM track_artists`); got != 3 {
		t.Fatalf("%d links, want all three tracks credited", got)
	}
}

// TestAlbumsWithTheSameTitleStayApart: "Greatest Hits" is not one album.
func TestAlbumsWithTheSameTitleStayApart(t *testing.T) {
	env := harness.New(t)
	seedFromExport(t, env, "collisions",
		[4]string{"trk0000000000000000001", "A", "Queen", "Greatest Hits"},
		[4]string{"trk0000000000000000002", "B", "ABBA", "Greatest Hits"},
	)
	if got := env.ScalarInt(`SELECT count(*) FROM albums`); got != 2 {
		t.Fatalf("%d albums for two different Greatest Hits, want 2", got)
	}
}

// TestEnrichmentOverwritesTheLocalCredit is the guard on the ordering rule.
//
// A local row is a floor, never a ceiling. When Spotify finally answers for a
// track, its credits replace the guessed one rather than joining it.
func TestEnrichmentOverwritesTheLocalCredit(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	worker := newEnrichWorker(t, env, fake, nil)

	trackID := "aaaaaaaaaaaaaaaaaaaaa1"
	seedFromExport(t, env, "enrichafter",
		[4]string{trackID, "Track " + trackID, "Guessed Artist", "Guessed Album"})

	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists WHERE artist_id LIKE 'local:%'`); got != 1 {
		t.Fatalf("%d local credits before enrichment, want 1", got)
	}

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}

	// The Spotify credit replaced it rather than sitting beside it: a track
	// credited to two artists would double-count in every chart.
	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists WHERE track_id = $1`, trackID); got != 1 {
		t.Fatalf("%d credits after enrichment, want exactly 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists WHERE track_id = $1 AND artist_id LIKE 'local:%'`,
		trackID); got != 0 {
		t.Fatal("the guessed credit survived alongside Spotify's")
	}
	// And the album moved to the real one.
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = $1 AND album_id LIKE 'local:%'`, trackID); got != 0 {
		t.Fatal("the track is still on the locally named album")
	}
}

// TestALocalArtistIsFoldedIntoItsSpotifyRow is the merge.
//
// Without it the same name appears twice in every chart with the plays split
// between them, and the split widens as enrichment progresses.
func TestALocalArtistIsFoldedIntoItsSpotifyRow(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	worker := newEnrichWorker(t, env, fake, nil)

	// The stub names every artist "Artist <id>", so the import is given the same
	// name for one of the two tracks. That track resolves; the other does not.
	resolving := "aaaaaaaaaaaaaaaaaaaaa1"
	stubName := "Artist " + artistIDFor(resolving)
	stranded := "bbbbbbbbbbbbbbbbbbbbb2"

	seedFromExport(t, env, "mergeuser",
		[4]string{resolving, "Resolved track", stubName, "An album"},
		[4]string{stranded, "Stranded track", stubName, "An album"},
	)
	if got := env.ScalarInt(`SELECT count(*) FROM artists`); got != 1 {
		t.Fatalf("%d artists after the import, want 1", got)
	}

	// Resolve only the first track, then its artist.
	env.Exec(`UPDATE tracks SET metadata_state = 'resolved' WHERE id = $1`, stranded)
	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}
	if _, err := worker.RunArtistsOnce(env.Ctx()); err != nil {
		t.Fatalf("run artists: %v", err)
	}

	// One artist, not two: the local row was folded into the Spotify one.
	if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE name = $1`, stubName); got != 1 {
		t.Fatalf("%d artists named %q after enrichment; a chart would show the name "+
			"twice with the plays split between them", got, stubName)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE metadata_state = 'local'`); got != 0 {
		t.Fatalf("%d local artists survived the merge", got)
	}

	// And the track that never resolved kept its credit, transferred to the real
	// artist row. Losing it would blank a track the import had already named.
	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists WHERE track_id = $1`, stranded); got != 1 {
		t.Fatal("the unresolved track lost its artist credit in the merge")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists ta JOIN artists a ON a.id = ta.artist_id
         WHERE ta.track_id = $1 AND a.metadata_state = 'resolved'`, stranded); got != 1 {
		t.Fatal("the unresolved track's credit did not move to the resolved artist")
	}
}

// TestHidingAnArtistSurvivesTheMerge: a user who hid an artist meant the artist,
// not the row that happened to represent them at the time.
func TestHidingAnArtistSurvivesTheMerge(t *testing.T) {
	env := harness.New(t)
	fake := newFakeSpotify(t)
	worker := newEnrichWorker(t, env, fake, nil)

	resolving := "aaaaaaaaaaaaaaaaaaaaa1"
	stubName := "Artist " + artistIDFor(resolving)
	seedFromExport(t, env, "hider", [4]string{resolving, "Track", stubName, "Album"})

	var user, localArtist string
	if err := env.Store.DB().QueryRow(env.Ctx(), `SELECT id FROM users LIMIT 1`).Scan(&user); err != nil {
		t.Fatalf("read the user: %v", err)
	}
	if err := env.Store.DB().QueryRow(env.Ctx(),
		`SELECT id FROM artists WHERE metadata_state = 'local'`).Scan(&localArtist); err != nil {
		t.Fatalf("read the local artist: %v", err)
	}
	env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, $2)`,
		user, localArtist)

	if _, err := worker.RunTracksOnce(env.Ctx()); err != nil {
		t.Fatalf("run tracks: %v", err)
	}
	if _, err := worker.RunArtistsOnce(env.Ctx()); err != nil {
		t.Fatalf("run artists: %v", err)
	}

	if got := env.ScalarInt(`SELECT count(*) FROM user_blacklisted_artists`); got != 1 {
		t.Fatalf("%d hidden artists after the merge, want 1 — hiding an artist must "+
			"not be undone by enrichment identifying them", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_blacklisted_artists b JOIN artists a ON a.id = b.artist_id
         WHERE a.metadata_state = 'resolved'`); got != 1 {
		t.Fatal("the hidden-artist entry did not move to the resolved row")
	}
}

// TestReplayingNamesRepairsAnOlderImport is what `encore-worker backfill-names`
// relies on.
//
// A history imported before Encore kept these names has tracks and listens but
// no artists at all, and no way to get them: the ids were never in the export.
// The names are still in the uploaded files, so the command reads them back and
// builds the catalogue that an import would build today. It must be able to do
// that against a database that has already been through everything.
func TestReplayingNamesRepairsAnOlderImport(t *testing.T) {
	env := harness.New(t)
	ctx, db := env.Ctx(), env.Store.DB()
	user := env.NewUser("olderimport")

	// An import as it used to be: track rows and listens, nothing else.
	old := []listens.TrackSeed{
		{ID: "trk0000000000000000001", Name: "Teardrop"},
		{ID: "trk0000000000000000002", Name: "Angel"},
	}
	if err := env.Listens.EnsureTracks(ctx, db, old); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
	at := time.Date(2021, time.June, 2, 8, 0, 0, 0, time.UTC)
	batch := make([]listens.StagedListen, 0, len(old))
	for i, s := range old {
		batch = append(batch, listens.Stage(domain.Listen{
			UserID: user.ID, PlayedAt: at.Add(time.Duration(i) * time.Hour),
			Precision: domain.PrecisionSecond,
			Identity:  domain.TrackIdentityFromID(s.ID),
			MsPlayed:  200_000, Source: domain.SourceExtended,
		}, nil))
	}
	if _, err := env.Listens.InsertListens(ctx, db, batch, "UTC"); err != nil {
		t.Fatalf("insert listens: %v", err)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM artists`); got != 0 {
		t.Fatalf("%d artists before the replay, want 0", got)
	}

	// The backfill replays the whole file, including plays too short to have been
	// stored — so it offers a track that is not in the database. That must not
	// take the batch down with it.
	replay := []listens.TrackSeed{
		{ID: "trk0000000000000000001", Name: "Teardrop", ArtistName: "Massive Attack", AlbumName: "Mezzanine"},
		{ID: "trk0000000000000000002", Name: "Angel", ArtistName: "Massive Attack", AlbumName: "Mezzanine"},
		{ID: "trk0000000000000000099", Name: "Skipped", ArtistName: "Nobody", AlbumName: "Nowhere"},
	}
	if err := env.Listens.EnsureLocalCatalogue(ctx, db, replay); err != nil {
		t.Fatalf("replay names: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists ta JOIN artists a ON a.id = ta.artist_id
         WHERE a.name = 'Massive Attack'`); got != 2 {
		t.Fatalf("%d tracks credited after the replay, want 2", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE album_id IS NOT NULL`); got != 2 {
		t.Fatalf("%d tracks gained an album, want 2", got)
	}
	// The track that was never imported got no credit, and no error either.
	if got := env.ScalarInt(
		`SELECT count(*) FROM track_artists WHERE track_id = 'trk0000000000000000099'`); got != 0 {
		t.Fatal("a credit was written for a track that is not in the database")
	}

	// Running it twice changes nothing.
	before := env.ScalarInt(`SELECT count(*) FROM track_artists`)
	if err := env.Listens.EnsureLocalCatalogue(ctx, db, replay); err != nil {
		t.Fatalf("second replay: %v", err)
	}
	if after := env.ScalarInt(`SELECT count(*) FROM track_artists`); after != before {
		t.Fatalf("a second replay took the credit count from %d to %d", before, after)
	}
}
