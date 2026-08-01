package stats

import "strings"

// DeviceUnknown is the bucket a device Spotify did not name falls into.
//
// Spotify's own vocabulary includes a literal "Unknown", and an observation can
// also arrive with an empty type. They are the same fact — the player did not
// say what it is — and share one key rather than producing an empty bar label
// beside a named one.
const DeviceUnknown = "unknown"

// DeviceFamily normalises one raw Spotify Connect device type.
//
// It lowercases, trims, and does nothing else. That is the whole difference
// between this and PlatformFamily beside it, and it is deliberate:
// listens.platform is free text an export writes in a shape no two vintages
// agree on, so grouping it is the only way to make it mean anything, while
// device_type is Spotify's own short enumeration — Computer, Smartphone,
// Speaker, TV, GameConsole, CastAudio — which already means something.
// Folding "CastAudio" into "speaker" would be Encore inventing an opinion about
// somebody's hardware and hiding the answer they asked for.
//
// A type Spotify adds later passes through unchanged and is counted, which is
// the same rule PlatformFamily's default follows and for the same reason: a
// category nobody has seen yet must still be counted, or the denominators stop
// adding up. Note that this is the opposite *shape* of rule from
// PlatformFamily's default, which buckets the unrecognised into "other" — there
// it is right, because the input is free text with no bound on its variety;
// here the input is an enumeration, so an unrecognised value is news and is
// worth showing by name.
//
// Grouping happens at read time and the raw string is never thrown away, so a
// future decision to bucket these reclassifies the whole history without a
// backfill.
func DeviceFamily(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == DeviceUnknown {
		return DeviceUnknown
	}
	return s
}
