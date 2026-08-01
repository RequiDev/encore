package stats

import "testing"

// TestDeviceFamilyNormalisesWithoutInventingAnOpinion pins the one difference
// between this classifier and PlatformFamily beside it.
//
// listens.platform is free text from an export — "Android OS 10 API 29
// (samsung, SM-G970F)", "Partner sonos_inc" — and needs a substring classifier
// to mean anything. device_type is Spotify Connect's own short enumeration, so
// the only work left is case and the absent value. Grouping "CastAudio" under
// "Speaker" would be Encore inventing an opinion about somebody's hardware, and
// a type Spotify adds tomorrow must be counted rather than dropped — for the
// reason PlatformFamily's default already gives: otherwise the denominators
// stop adding up.
//
// Fails when: the default case starts returning a bucket instead of the value
// itself, so a device type Spotify adds is silently folded into "other" and the
// breakdown stops naming what somebody actually played on; or the empty case
// stops mapping to "unknown", at which point an empty bar label reaches a chart.
func TestDeviceFamilyNormalisesWithoutInventingAnOpinion(t *testing.T) {
	for raw, want := range map[string]string{
		"Computer":    "computer",
		"Smartphone":  "smartphone",
		"Tablet":      "tablet",
		"Speaker":     "speaker",
		"TV":          "tv",
		"AVR":         "avr",
		"STB":         "stb",
		"AudioDongle": "audiodongle",
		"GameConsole": "gameconsole",
		"CastVideo":   "castvideo",
		"CastAudio":   "castaudio",
		"Automobile":  "automobile",
		"  Speaker  ": "speaker",
		"Unknown":     DeviceUnknown,
		"":            DeviceUnknown,
		// A type Spotify has not minted yet. It must arrive named, not bucketed:
		// this is the case that fails if the default ever becomes a catch-all.
		"HoloProjector": "holoprojector",
	} {
		if got := DeviceFamily(raw); got != want {
			t.Errorf("DeviceFamily(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestDeviceFamilyIsNotPlatformFamily is the assertion the two classifiers
// existing side by side actually needs.
//
// They take the same argument type and return the same one, so nothing but a
// test stops a later edit from deleting one and pointing its callers at the
// other. On Spotify Connect's own vocabulary they disagree on every value:
// PlatformFamily is a substring classifier built for an export's free text, so
// it reads "CastAudio" as the cast family and answers "other" for everything
// else it has never seen — which is most of this list.
//
// Fails when: DeviceFamily is replaced by, or reimplemented as, PlatformFamily.
func TestDeviceFamilyIsNotPlatformFamily(t *testing.T) {
	for _, raw := range []string{"Computer", "Smartphone", "Speaker", "GameConsole", "TV"} {
		if DeviceFamily(raw) == PlatformFamily(raw) {
			t.Errorf("DeviceFamily(%q) and PlatformFamily(%q) both answered %q; "+
				"device_type is Spotify Connect's vocabulary and platform is an export's "+
				"free text, and the two must never be classified by the same rule",
				raw, raw, DeviceFamily(raw))
		}
	}
}
