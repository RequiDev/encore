package imports

import (
	"strings"
	"testing"
)

// TestCheckpointSQLGuard pins the three properties of the checkpoint statement
// that cannot be checked without a database but that everything else depends on:
// it never moves a file's position backwards, it requires strict progress so a
// retried batch cannot add its counters twice, and it writes the byte offset it
// was given rather than keeping an older one that would no longer correspond to
// the record offset beside it.
func TestCheckpointSQLGuard(t *testing.T) {
	if !strings.Contains(checkpointSQL, "WHERE id = $1 AND record_offset < $2") {
		t.Fatal("checkpointSQL lost its monotonic guard; a stale retry could rewind the checkpoint")
	}
	if strings.Contains(checkpointSQL, "record_offset <= $2") {
		t.Fatal("checkpointSQL must require strict progress: with <=, a batch whose commit " +
			"acknowledgement was lost would re-apply its counters on retry and double-count")
	}
	if !strings.Contains(checkpointSQL, "byte_offset = $3") {
		t.Fatal("checkpointSQL must assign byte_offset directly, including NULL")
	}
	for _, add := range []string{
		"imported = imported + $4",
		"duplicates = duplicates + $5",
		"skipped = skipped + $6",
		"rejected = rejected + $7",
	} {
		if !strings.Contains(checkpointSQL, add) {
			t.Fatalf("checkpointSQL must accumulate counters (%q), not replace them", add)
		}
	}
}

// TestVerificationDataCountsTheTable asserts that verification asks the fact
// table how many listens exist rather than trusting the importer's own tally,
// which is the only reason the check has any value.
func TestVerificationDataCountsTheTable(t *testing.T) {
	if !strings.Contains(verificationDataSQL, "LEFT JOIN listens l ON l.import_file_id = f.id") {
		t.Fatal("verificationDataSQL must count rows in listens per import file")
	}
	if !strings.Contains(verificationDataSQL, "count(l.id)") {
		t.Fatal("verificationDataSQL must report a real count, not a stored counter")
	}
}
