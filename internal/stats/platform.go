package stats

import "strings"

// The platform families a listen is grouped into.
//
// listens.platform is free text straight from the export — "Android OS 10 API 29
// (samsung, SM-G970F)", "OS X 10.15.7 [x86 8]", "Partner sonos_inc" — and no two
// vintages agree on its shape. Grouping happens at read time rather than at
// import, so adding a family reclassifies the whole history without a backfill
// and without ever having thrown the original string away.
const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
	PlatformWindows = "windows"
	PlatformMacOS   = "macos"
	PlatformLinux   = "linux"
	PlatformWeb     = "web"
	PlatformCast    = "cast"
	PlatformPartner = "partner"
	PlatformOther   = "other"
)

// PlatformFamily groups one raw platform string.
//
// The order of the cases is the whole design. A partner integration string can
// name the operating system underneath it, and it is a partner device first, so
// that case comes before every OS. Anything unrecognised is Other rather than
// dropped: a family nobody has seen yet must still be counted, or the
// denominators stop adding up.
func PlatformFamily(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case s == "" || s == "not_applicable" || s == "unknown":
		return PlatformOther
	case strings.Contains(s, "partner"):
		return PlatformPartner
	case strings.Contains(s, "cast"):
		return PlatformCast
	case strings.Contains(s, "web_player"), strings.Contains(s, "webplayer"), strings.Contains(s, "web player"):
		return PlatformWeb
	case strings.Contains(s, "android"):
		return PlatformAndroid
	// Prefix rather than substring: "ios" appears inside ordinary words, and a
	// real iOS platform string always begins with it.
	case strings.HasPrefix(s, "ios"), strings.Contains(s, "iphone"), strings.Contains(s, "ipad"):
		return PlatformIOS
	case strings.Contains(s, "windows"):
		return PlatformWindows
	case strings.Contains(s, "os x"), strings.Contains(s, "macos"), strings.Contains(s, "osx"):
		return PlatformMacOS
	case strings.Contains(s, "linux"):
		return PlatformLinux
	default:
		return PlatformOther
	}
}
