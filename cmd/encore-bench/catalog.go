package main

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/RequiDev/encore/internal/domain"
)

// Catalogue dimensions. A real listener's history is not a uniform draw over an
// unbounded catalogue: it is a few hundred artists and a few thousand tracks
// played over and over with a very heavy skew. Reproducing that shape matters
// for the benchmark, because it is what decides how much work the importer's
// track and alias upserts actually do — a synthetic export with a million
// distinct track ids would measure something Encore never sees.
const (
	catalogArtists = 400
	catalogAlbums  = 1200
	catalogTracks  = 5000
	catalogShows   = 25
	// episodesPerShow keeps the podcast side small: podcasts exist in this
	// dataset only so that the importer's not-music skip path is exercised.
	episodesPerShow = 40
)

// zipfExponent shapes how sharply plays concentrate on the most-played tracks.
// 1.1 gives the long, heavy tail that real listening histories have: a handful
// of tracks account for a large share of plays and most of the catalogue is
// touched only a few times.
const zipfExponent = 1.1

// base62 is the alphabet of a Spotify id. Ids generated from it satisfy
// domain.TrackIDFromURI, so a generated export exercises the same resolved
// identity path as a genuine extended export.
const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// spotifyIDLength is the id length Spotify has always used in practice.
const spotifyIDLength = 22

// synthTrack is one track of the synthetic catalogue.
type synthTrack struct {
	ID         string
	Title      string
	Artist     string
	Album      string
	DurationMs int32
}

// synthShow is one podcast, which exists only to be skipped.
type synthShow struct {
	Name     string
	Episodes []synthEpisode
}

// synthEpisode is one podcast episode.
type synthEpisode struct {
	ID    string
	Title string
}

// synthCatalog is the deterministic universe a generated export draws from.
//
// It is built once, in memory, and is deliberately tiny compared with the export
// it produces: the point of the generator is that record *emission* streams, not
// that the catalogue does.
type synthCatalog struct {
	tracks []synthTrack
	shows  []synthShow
	// picker draws a track index with a Zipf-like skew.
	picker *rand.Zipf
	// rng is the catalogue's own stream, which the pickers keep drawing from once
	// the catalogue is built. Keeping it separate from the stream that shapes each
	// record means the two never interfere: the same seed picks the same tracks in
	// the same order whether a hundred records are generated or a million.
	rng *rand.Rand
}

// newSynthCatalog builds the catalogue from a seeded generator. The same seed
// always produces the same catalogue, which is what makes a benchmark run
// reproducible rather than merely repeatable.
func newSynthCatalog(rng *rand.Rand) *synthCatalog {
	artists := makeUnique(rng, catalogArtists, artistName, domain.NormalizeArtist)
	albums := makeUnique(rng, catalogAlbums, workTitle, domain.NormalizeTitle)
	titles := makeUnique(rng, catalogTracks, workTitle, domain.NormalizeTitle)

	// Artists and albums are themselves drawn with a skew, so that a few artists
	// own a large slice of the catalogue exactly as they do in a real library.
	artistPick := rand.NewZipf(rng, zipfExponent, 1, uint64(len(artists)-1))
	albumPick := rand.NewZipf(rng, zipfExponent, 1, uint64(len(albums)-1))

	tracks := make([]synthTrack, len(titles))
	for i, title := range titles {
		tracks[i] = synthTrack{
			ID:     randomID(rng),
			Title:  title,
			Artist: artists[artistPick.Uint64()],
			Album:  albums[albumPick.Uint64()],
			// Two minutes to seven, which is where the overwhelming majority of
			// released tracks sit.
			DurationMs: int32(120_000 + rng.IntN(300_000)),
		}
	}

	shows := make([]synthShow, catalogShows)
	showNames := makeUnique(rng, catalogShows, showName, domain.NormalizeText)
	for i := range shows {
		eps := make([]synthEpisode, episodesPerShow)
		for j := range eps {
			eps[j] = synthEpisode{
				ID:    randomID(rng),
				Title: fmt.Sprintf("%d. %s", j+1, workTitle(rng)),
			}
		}
		shows[i] = synthShow{Name: showNames[i], Episodes: eps}
	}

	return &synthCatalog{
		tracks: tracks,
		shows:  shows,
		picker: rand.NewZipf(rng, zipfExponent, 1, uint64(len(tracks)-1)),
		rng:    rng,
	}
}

// pickTrack returns the index of the next track to be played.
func (c *synthCatalog) pickTrack() int { return int(c.picker.Uint64()) }

// pickEpisode returns a podcast episode and the show it belongs to.
func (c *synthCatalog) pickEpisode() (synthShow, synthEpisode) {
	show := c.shows[c.rng.IntN(len(c.shows))]
	return show, show.Episodes[c.rng.IntN(len(show.Episodes))]
}

// randomID mints a Spotify-shaped identifier.
func randomID(rng *rand.Rand) string {
	var b [spotifyIDLength]byte
	for i := range b {
		b[i] = base62[rng.IntN(len(base62))]
	}
	return string(b[:])
}

// makeUnique draws n names from gen, which is allowed to collide, and returns a
// set that is distinct under key.
//
// Distinctness is measured after normalisation, not on the raw string, because
// normalisation is where Encore compares names: an account-data export has no
// track URIs, so its listens are identified by the normalised artist and title
// alone, and two catalogue entries that fold together there would not be two
// entries as far as the database is concerned.
func makeUnique(rng *rand.Rand, n int, gen func(*rand.Rand) string, key func(string) string) []string {
	seen := make(map[string]struct{}, n)
	out := make([]string, 0, n)
	for len(out) < n {
		base := gen(rng)
		name := base
		// A digit survives normalisation, so numbering a repeat the way a
		// catalogue numbers two bands of the same name keeps the entries apart
		// exactly where Encore looks. It also terminates, which redrawing does not
		// promise once the name space is nearly exhausted.
		for i := 2; ; i++ {
			if _, dup := seen[key(name)]; !dup {
				break
			}
			name = fmt.Sprintf("%s %d", base, i)
		}
		seen[key(name)] = struct{}{}
		out = append(out, name)
	}
	return out
}

// --- name generation -------------------------------------------------------

var artistFirstWords = []string{
	"Velvet", "Northern", "Paper", "Glass", "Golden", "Quiet", "Electric", "Midnight",
	"Silver", "Hollow", "Crimson", "Wandering", "Steady", "Distant", "Wild", "Patient",
	"Small", "Bright", "Slow", "Endless", "Modern", "Ancient", "Careful", "Restless",
}

var artistSecondWords = []string{
	"Ledger", "Static", "Harbour", "Cartel", "Orchard", "Machine", "Sisters", "Brothers",
	"Signal", "Anchor", "Circus", "Parade", "Chorus", "Foxes", "Wolves", "Lantern",
	"Compass", "Tundra", "Meridian", "Aviary", "Foundry", "Almanac", "Cartographer", "Ferry",
}

var titleAdjectives = []string{
	"Quiet", "Broken", "Certain", "Familiar", "Distant", "Honest", "Late", "Early",
	"Winter", "Summer", "Northern", "Southern", "Second", "Last", "Open", "Narrow",
	"Bright", "Heavy", "Tender", "Sudden", "Long", "Small", "Perfect", "Ordinary",
}

var titleNouns = []string{
	"Harbour", "Letter", "Morning", "Weather", "Answer", "Ceiling", "Photograph", "Station",
	"Argument", "Kitchen", "Motorway", "Cathedral", "Postcard", "Balcony", "Telephone", "Garden",
	"Summer", "Winter", "Silence", "Applause", "Departure", "Arrival", "Signal", "Shoreline",
	"Fever", "Anchor", "Corridor", "Rooftop", "Whisper", "Machine", "Suitcase", "Lighthouse",
}

var titleConnectors = []string{"of the", "in the", "and the", "under the", "before the", "after the"}

// editionSuffixes are the release markers Spotify puts in album names. They are
// here because domain.NormalizeTitle strips exactly these, so their presence
// keeps the generated data honest about what the importer has to cope with.
var editionSuffixes = []string{
	" (Deluxe Edition)", " - Remastered 2011", " (Anniversary Edition)", " - Remastered",
}

// artistName builds one plausible band or performer name.
func artistName(rng *rand.Rand) string {
	first := artistFirstWords[rng.IntN(len(artistFirstWords))]
	second := artistSecondWords[rng.IntN(len(artistSecondWords))]
	switch rng.IntN(4) {
	case 0:
		return "The " + first + " " + second
	case 1:
		return first + " " + second
	case 2:
		return second
	default:
		return first + " " + strings.ToLower(second)
	}
}

// workTitle builds one plausible track or album title.
func workTitle(rng *rand.Rand) string {
	adj := titleAdjectives[rng.IntN(len(titleAdjectives))]
	noun := titleNouns[rng.IntN(len(titleNouns))]
	other := titleNouns[rng.IntN(len(titleNouns))]
	conn := titleConnectors[rng.IntN(len(titleConnectors))]
	switch rng.IntN(5) {
	case 0:
		return adj + " " + noun
	case 1:
		return noun + " " + conn + " " + other
	case 2:
		return adj + " " + noun + " " + conn + " " + other
	case 3:
		return noun
	default:
		return "The " + adj + " " + noun
	}
}

// albumTitleFor decorates a track's album with an edition marker every so often.
func albumTitleFor(rng *rand.Rand, album string) string {
	if rng.IntN(8) != 0 {
		return album
	}
	return album + editionSuffixes[rng.IntN(len(editionSuffixes))]
}

// showName builds one podcast name.
func showName(rng *rand.Rand) string {
	return titleNouns[rng.IntN(len(titleNouns))] + " " +
		titleNouns[rng.IntN(len(titleNouns))] + " Podcast"
}
