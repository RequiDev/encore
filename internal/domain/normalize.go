package domain

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// editionSuffixes are parenthetical or dash-separated markers that describe a
// *release edition* rather than a different recording. Stripping them lets the
// same song match across a Spotify catalogue entry ("Song") and an account-data
// export row ("Song - Remastered 2011").
//
// Deliberately excluded: live, remix, acoustic, instrumental, demo, cover,
// karaoke, edit, mix, reprise and version-with-a-qualifier. Those denote genuinely
// different recordings and merging them would silently corrupt statistics.
var editionSuffixes = []string{
	"remaster",
	"remastered",
	"remastered version",
	"digital remaster",
	"digitally remastered",
	"album version",
	"original album version",
	"bonus track",
	"bonus",
	"deluxe",
	"deluxe edition",
	"deluxe version",
	"expanded edition",
	"anniversary edition",
	"special edition",
	"explicit",
	"explicit version",
	"clean",
	"clean version",
	"mono",
	"mono version",
	"stereo",
	"stereo version",
	"stereo mix",
	"remaster 2009",
}

// NormalizeText folds a string into a stable comparison form: NFKC composition,
// lowercase, punctuation-insensitive whitespace, single spaces, trimmed.
//
// It is used for every name comparison in Encore, so it must stay deterministic
// across releases: changing it changes identity keys. See docs/import.md.
func NormalizeText(s string) string {
	if s == "" {
		return ""
	}
	s = norm.NFKC.String(s)
	s = strings.ToLower(s)

	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || r == '_':
			space = true
		case r == '’' || r == 'ʼ' || r == '`' || r == '´':
			// Curly apostrophes and their lookalikes fold to the ASCII form so
			// "Don't" and "Don’t" compare equal.
			b.WriteByte('\'')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '&' || r == '+':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			// Any other punctuation acts as a separator rather than being dropped,
			// so "rock/pop" does not become "rockpop".
			space = true
		}
	}
	return b.String()
}

// NormalizeArtist folds an artist name for comparison.
func NormalizeArtist(s string) string {
	return NormalizeText(s)
}

// NormalizeTitle folds a track or album title for comparison, additionally
// removing edition markers such as "- Remastered 2011" or "(Deluxe Edition)".
func NormalizeTitle(s string) string {
	s = stripEditionSuffixes(s)
	return NormalizeText(s)
}

// stripEditionSuffixes repeatedly removes trailing edition markers, so
// "Song - Remastered 2011 (Deluxe Edition)" reduces to "Song".
func stripEditionSuffixes(s string) string {
	for range 4 {
		trimmed := stripOneEditionSuffix(s)
		if trimmed == s {
			return s
		}
		s = trimmed
	}
	return s
}

func stripOneEditionSuffix(s string) string {
	t := strings.TrimSpace(s)

	// Bracketed form: "Song (Remastered 2011)" / "Song [Deluxe Edition]".
	if len(t) > 0 {
		var open, close byte
		switch t[len(t)-1] {
		case ')':
			open, close = '(', ')'
		case ']':
			open, close = '[', ']'
		}
		if close != 0 {
			if i := strings.LastIndexByte(t, open); i > 0 {
				if isEditionMarker(t[i+1 : len(t)-1]) {
					return strings.TrimSpace(t[:i])
				}
			}
		}
	}

	// Dash form: "Song - Remastered 2011". Only the final segment is considered,
	// and only when a separator with surrounding spaces is present, so hyphenated
	// titles like "Post-Punk" survive.
	for _, sep := range []string{" - ", " – ", " — "} {
		if i := strings.LastIndex(t, sep); i > 0 {
			if isEditionMarker(t[i+len(sep):]) {
				return strings.TrimSpace(t[:i])
			}
		}
	}
	return s
}

// isEditionMarker reports whether a suffix segment is an edition marker, allowing
// an optional trailing or leading 4-digit year ("Remastered 2011", "2011 Remaster").
func isEditionMarker(seg string) bool {
	c := strings.ToLower(strings.TrimSpace(seg))
	c = strings.Trim(c, ".!")
	if c == "" {
		return false
	}
	// Peel a leading or trailing year.
	if y, rest, ok := splitYear(c); ok {
		_ = y
		c = strings.TrimSpace(rest)
	}
	if c == "" {
		return false
	}
	for _, m := range editionSuffixes {
		if c == m {
			return true
		}
	}
	return false
}

func splitYear(s string) (year, rest string, ok bool) {
	isYear := func(w string) bool {
		if len(w) != 4 {
			return false
		}
		for i := range w {
			if w[i] < '0' || w[i] > '9' {
				return false
			}
		}
		return w[0] == '1' || w[0] == '2'
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return "", s, false
	}
	if isYear(fields[0]) {
		return fields[0], strings.Join(fields[1:], " "), true
	}
	if isYear(fields[len(fields)-1]) {
		return fields[len(fields)-1], strings.Join(fields[:len(fields)-1], " "), true
	}
	return "", s, false
}

// TrackIDFromURI extracts the bare id from a Spotify URI or open.spotify.com URL.
// It returns ("", false) for local files, podcast episodes and anything that is
// not a track reference.
//
//	spotify:track:4uLU6hMCjMI75M1A2tKUQC          -> 4uLU6hMCjMI75M1A2tKUQC, true
//	https://open.spotify.com/track/4uLU6...?si=x  -> 4uLU6...,               true
//	spotify:local:Artist:Album:Title:213          -> "",                     false
func TrackIDFromURI(uri string) (string, bool) {
	u := strings.TrimSpace(uri)
	if u == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(u, "spotify:track:"); ok {
		return validTrackID(rest)
	}
	for _, prefix := range []string{"https://open.spotify.com/track/", "http://open.spotify.com/track/", "open.spotify.com/track/"} {
		if rest, ok := strings.CutPrefix(u, prefix); ok {
			if i := strings.IndexAny(rest, "?#/"); i >= 0 {
				rest = rest[:i]
			}
			return validTrackID(rest)
		}
	}
	// Intl paths such as open.spotify.com/intl-de/track/<id>.
	if i := strings.Index(u, "/track/"); i >= 0 && strings.Contains(u, "spotify.com") {
		rest := u[i+len("/track/"):]
		if j := strings.IndexAny(rest, "?#/"); j >= 0 {
			rest = rest[:j]
		}
		return validTrackID(rest)
	}
	return "", false
}

// validTrackID checks the base-62 shape of a Spotify id without asserting a fixed
// length, since Spotify has never formally guaranteed 22 characters.
func validTrackID(s string) (string, bool) {
	if len(s) < 10 || len(s) > 64 {
		return "", false
	}
	for i := range s {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		return "", false
	}
	return s, true
}
