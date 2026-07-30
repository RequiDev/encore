package spotify

import (
	"slices"
	"testing"
)

func TestMissingScopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		granted []string
		want    []string
		missing []string
	}{
		{"nothing granted", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"all granted", []string{"a", "b"}, []string{"a", "b"}, []string{}},
		{"some granted", []string{"b"}, []string{"a", "b", "c"}, []string{"a", "c"}},
		{"extra granted is not missing", []string{"a", "b", "z"}, []string{"a"}, []string{}},
		{"nothing wanted", []string{"a"}, nil, []string{}},
		// Spotify returns granted scopes space-separated in one string, and the
		// stored column has held them that way for accounts connected before the
		// value was split. Both shapes must work.
		{"space separated", []string{"a b"}, []string{"a", "b"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingScopes(tc.granted, tc.want)
			if got == nil {
				t.Fatal("MissingScopes returned nil; it must return an empty slice so the JSON is [] and not null")
			}
			if !slices.Equal(got, tc.missing) {
				t.Errorf("got %v, want %v", got, tc.missing)
			}
		})
	}
}

// TestMissingScopesPreservesWantOrder keeps the prompt's wording stable: the
// order the scopes are listed in is the order they are explained to the user.
func TestMissingScopesPreservesWantOrder(t *testing.T) {
	got := MissingScopes(nil, []string{"c", "a", "b"})
	if !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Errorf("got %v, want the wanted order preserved", got)
	}
}
