package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envName matches a configuration variable. The trailing character class is what
// keeps it from matching the bare prefix where the word appears in prose.
var envName = regexp.MustCompile(`ENCORE_[A-Z0-9]+(_[A-Z0-9]+)*`)

func namesIn(t *testing.T, parts ...string) map[string]bool {
	t.Helper()
	path := filepath.Join(parts...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, name := range envName.FindAllString(string(body), -1) {
		out[name] = true
	}
	return out
}

// TestComposeForwardsEveryConfigurationVariable is the guard on a failure that
// looks exactly like an application bug.
//
// Compose forwards precisely the variables the environment block names and drops
// everything else without a word. A setting that is documented, spelled
// correctly and present in the .env file therefore has no effect at all if
// nobody added it here — and the symptom is a feature that silently does
// nothing, which is indistinguishable from the feature being broken.
//
// That is not hypothetical: the metadata fallback shipped with its variables
// documented, in .env.example, and absent from the compose file, so configuring
// it did nothing whatsoever. Forty-two other settings were in the same state.
func TestComposeForwardsEveryConfigurationVariable(t *testing.T) {
	read := namesIn(t, "..", "..", "internal", "config", "config.go")
	forwarded := namesIn(t, "..", "..", "docker-compose.yml")

	var missing []string
	for name := range read {
		// The bare prefix appears in the package comment.
		if name == "ENCORE_" || !strings.HasPrefix(name, "ENCORE_") {
			continue
		}
		if !forwarded[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("internal/config reads %d variables that docker-compose.yml does not pass "+
			"to the containers, so setting them has no effect:\n  %s\n\n"+
			"Add each to the x-encore-env anchor as `NAME: ${NAME:-}`. An empty value is "+
			"treated as unset by the parser, so a line for a setting nobody uses costs nothing.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestEnvExampleDocumentsWhatComposeForwards keeps the two halves of the
// operator's story together: if a variable can be set, the example file should
// say so, or nobody will know it exists.
func TestEnvExampleDocumentsWhatComposeForwards(t *testing.T) {
	documented := namesIn(t, "..", "..", ".env.example")
	forwarded := namesIn(t, "..", "..", "docker-compose.yml")

	// Variables the compose file sets itself rather than accepting from the
	// operator. Putting these in .env.example would invite somebody to set one
	// and wonder why it was ignored.
	fixed := map[string]bool{
		"ENCORE_DATABASE_URL":        true, // built from POSTGRES_PASSWORD
		"ENCORE_IMPORT_DIR":          true, // a path inside the container
		"ENCORE_TRUST_PROXY_HEADERS": true, // nginx always sits in front
	}

	var undocumented []string
	for name := range forwarded {
		if fixed[name] || documented[name] {
			continue
		}
		undocumented = append(undocumented, name)
	}
	sort.Strings(undocumented)

	if len(undocumented) > 0 {
		t.Fatalf("docker-compose.yml forwards %d variables that .env.example never mentions:\n  %s",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
}
