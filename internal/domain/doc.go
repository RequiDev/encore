// Package domain holds Encore's core types and rules: what a listening event is,
// how a listen is identified, how duplicates are decided, and how imports are
// accounted for.
//
// The package performs no I/O and knows nothing about HTTP, PostgreSQL, Spotify's
// wire format or background jobs. Its only non-standard-library dependencies are
// pure-function packages: golang.org/x/text for Unicode normalisation and
// github.com/google/uuid for identifier values. This is enforced by TestNoIODependencies
// in deps_test.go.
//
// Everything in here is deterministic and cheap, which is what makes the duplicate
// strategy testable in isolation from a database.
package domain
