package domain

import (
	"strings"
	"testing"
)

func TestLocalArtistIDIsStableAcrossSpellings(t *testing.T) {
	want := LocalArtistID("Massive Attack")
	if want == "" {
		t.Fatal("LocalArtistID returned nothing for a perfectly good name")
	}
	for _, spelling := range []string{
		"massive attack", "MASSIVE ATTACK", "  Massive   Attack  ", "Massive\tAttack",
	} {
		if got := LocalArtistID(spelling); got != want {
			t.Errorf("LocalArtistID(%q) = %q, want %q — a catalogue would fracture "+
				"into one artist per spelling", spelling, got, want)
		}
	}
}

func TestLocalArtistIDSeparatesDifferentArtists(t *testing.T) {
	if LocalArtistID("Massive Attack") == LocalArtistID("Portishead") {
		t.Fatal("two artists collided")
	}
}

func TestLocalIDsRefuseEmptyNames(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if got := LocalArtistID(name); got != "" {
			t.Errorf("LocalArtistID(%q) = %q, want \"\"", name, got)
		}
		if got := LocalAlbumID("Someone", name); got != "" {
			t.Errorf("LocalAlbumID(_, %q) = %q, want \"\"", name, got)
		}
	}
	// An album with no artist is still an album: only the title is required.
	if LocalAlbumID("", "Untitled") == "" {
		t.Error("an album with no credited artist got no id")
	}
}

// TestLocalAlbumIDIsKeyedOnTheArtist is the one that matters for a real history:
// a library of any size holds several unrelated Greatest Hits.
func TestLocalAlbumIDIsKeyedOnTheArtist(t *testing.T) {
	queen := LocalAlbumID("Queen", "Greatest Hits")
	abba := LocalAlbumID("ABBA", "Greatest Hits")
	if queen == abba {
		t.Fatal("two different albums with the same title collided into one row")
	}
	if queen != LocalAlbumID("queen", "greatest hits") {
		t.Fatal("the same album under a different spelling got a second row")
	}
}

func TestLocalAndSpotifyIDsAreDistinguishable(t *testing.T) {
	id := LocalArtistID("Massive Attack")
	if !IsLocalID(id) {
		t.Fatalf("IsLocalID(%q) = false", id)
	}
	// A Spotify id is base-62 and can never contain a colon, which is what makes
	// the two kinds unmistakable everywhere they are read.
	if !strings.Contains(id, ":") {
		t.Fatalf("local id %q has nothing a Spotify id could not have", id)
	}
	for _, spotifyID := range []string{"4cOdK2wGLETKBW3PvgPWqT", "0gxyHStUsqpMadRV0Di1Qt"} {
		if IsLocalID(spotifyID) {
			t.Errorf("IsLocalID(%q) = true", spotifyID)
		}
	}
}

// TestLocalIDsAreKindSpecific: an artist and an album called the same thing must
// not share a row, and they are different tables with one id space each.
func TestLocalIDsAreKindSpecific(t *testing.T) {
	if LocalArtistID("Weezer") == LocalAlbumID("Weezer", "Weezer") {
		t.Fatal("an artist and their self-titled album derived the same id")
	}
}
