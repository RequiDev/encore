package stats

import "testing"

// TestPlatformFamily pins the classifier against the shapes Spotify's exports
// actually contain. The strings below are the real formats, not invented ones.
func TestPlatformFamily(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"Android OS 10 API 29 (samsung, SM-G970F)", PlatformAndroid},
		{"android", PlatformAndroid},
		{"iOS 14.4.2 (iPhone12,1)", PlatformIOS},
		{"iOS 9.3.5 (iPad4,1)", PlatformIOS},
		{"Windows 10 (10.0.19042; x64)", PlatformWindows},
		{"windows", PlatformWindows},
		{"OS X 10.15.7 [x86 8]", PlatformMacOS},
		{"macos", PlatformMacOS},
		{"Linux [x86_64 0]", PlatformLinux},
		{"web_player", PlatformWeb},
		{"WebPlayer", PlatformWeb},
		{"cast", PlatformCast},
		{"Google Cast", PlatformCast},
		{"Partner sonos_inc bridge", PlatformPartner},
		{"not_applicable", PlatformOther},
		{"unknown", PlatformOther},
		{"", PlatformOther},
		{"something nobody has seen", PlatformOther},
	} {
		if got := PlatformFamily(tc.raw); got != tc.want {
			t.Errorf("PlatformFamily(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestPlatformFamilyPrefersTheMoreSpecificMatch is why the switch is ordered.
// A partner integration string can name the underlying OS, and it is a partner
// device first.
func TestPlatformFamilyPrefersTheMoreSpecificMatch(t *testing.T) {
	if got := PlatformFamily("Partner android_auto"); got != PlatformPartner {
		t.Errorf("a partner string naming Android classified as %q, want %q", got, PlatformPartner)
	}
	if got := PlatformFamily("Partner google cast"); got != PlatformPartner {
		t.Errorf("a partner string naming Cast classified as %q, want %q", got, PlatformPartner)
	}
}
