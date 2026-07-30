package spotify

import "strings"

// MissingScopes reports which of want a grant does not include, in want's own
// order, and never returns nil.
//
// It tolerates the two shapes a stored grant can take. Spotify returns granted
// scopes space-separated in a single string, and an account connected before
// Encore split them has one such string in its scopes column; a newer one has
// them as separate elements. Both are flattened here rather than at each call
// site, which is the same reason HasScope splits on spaces.
func MissingScopes(granted, want []string) []string {
	have := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		for f := range strings.SplitSeq(g, " ") {
			if f != "" {
				have[f] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(want))
	for _, w := range want {
		if _, ok := have[w]; !ok {
			missing = append(missing, w)
		}
	}
	return missing
}
