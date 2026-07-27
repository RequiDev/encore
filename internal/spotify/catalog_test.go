package spotify

import (
	"testing"

	"github.com/RequiDev/encore/internal/domain"
)

// TestLocalIDsNeverReachSpotify is the safety property behind the local
// catalogue: ids Encore mints from an export's names are not Spotify ids, and an
// enrichment batch that happened to carry one must drop it rather than send a
// request that could only be answered with a 400.
func TestLocalIDsNeverReachSpotify(t *testing.T) {
	for _, id := range []string{
		domain.LocalArtistID("Massive Attack"),
		domain.LocalAlbumID("Massive Attack", "Mezzanine"),
	} {
		if id == "" {
			t.Fatal("the local id helper returned nothing")
		}
		if validID(id) {
			t.Errorf("validID(%q) = true; a local id would be sent to Spotify", id)
		}
		if got := cleanIDs([]string{id, "4cOdK2wGLETKBW3PvgPWqT"}); len(got) != 1 ||
			got[0] != "4cOdK2wGLETKBW3PvgPWqT" {
			t.Errorf("cleanIDs kept %v; the local id survived the filter", got)
		}
	}
}
