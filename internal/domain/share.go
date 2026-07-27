package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ShareMaxLabelLength bounds the name an owner gives a link. It is shown on the
// shared page, so it is the one piece of free text a share can carry.
const ShareMaxLabelLength = 80

// ShareMaxDays caps a rolling window at roughly ten years, which is longer than
// any history worth calling a window and short enough to keep the arithmetic
// honest.
const ShareMaxDays = 3660

// ShareLink is a read-only, revocable link to one user's aggregate statistics.
//
// What a share can expose is fixed by the feature rather than by the row: totals,
// charts and rankings, never individual plays. There is no field here that could
// widen it, which is deliberate — a privacy boundary that depends on a boolean
// being set correctly is one that will eventually be set incorrectly.
type ShareLink struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Label  string

	// From and To pin the range. Both zero means all time.
	From time.Time
	To   time.Time
	// Days, when positive, means a window of that many days ending now, and
	// From/To are unset. A rolling share stays current without being edited.
	Days int

	ExpiresAt    time.Time
	RevokedAt    time.Time
	LastViewedAt time.Time
	ViewCount    int64
	CreatedAt    time.Time
}

// Active reports whether the link still answers.
func (s ShareLink) Active(now time.Time) bool {
	if !s.RevokedAt.IsZero() {
		return false
	}
	return s.ExpiresAt.IsZero() || s.ExpiresAt.After(now)
}

// Rolling reports whether the range moves with time.
func (s ShareLink) Rolling() bool { return s.Days > 0 }

// Range resolves the link's window at a given instant.
//
// A rolling link is computed from now, which is what makes "the last ninety
// days" mean the same thing to a visitor next month as it does today. An
// all-time link is anchored at the epoch rather than left zero, because every
// statistics query insists on two real bounds.
func (s ShareLink) Range(now time.Time, firstListen time.Time) TimeRange {
	switch {
	case s.Rolling():
		return TimeRange{From: now.Add(-time.Duration(s.Days) * 24 * time.Hour), To: now}
	case !s.From.IsZero() && !s.To.IsZero():
		return TimeRange{From: s.From, To: s.To}
	}

	from := firstListen
	if from.IsZero() {
		// Nothing listened to yet. Any range is empty; this one is cheap.
		from = now.Add(-24 * time.Hour)
	}
	return TimeRange{From: from, To: now}
}

// ValidateShare checks a link an owner is asking to create.
func ValidateShare(label string, from, to time.Time, days int, expires time.Time, now time.Time) error {
	if len([]rune(label)) > ShareMaxLabelLength {
		return fmt.Errorf("%w: the label may be at most %d characters",
			ErrValidation, ShareMaxLabelLength)
	}

	hasRange := !from.IsZero() || !to.IsZero()
	switch {
	case days > 0 && hasRange:
		return fmt.Errorf("%w: a link covers either a fixed range or a rolling window, not both",
			ErrValidation)
	case days < 0 || days > ShareMaxDays:
		return fmt.Errorf("%w: a rolling window must be between 1 and %d days",
			ErrValidation, ShareMaxDays)
	case hasRange && (from.IsZero() || to.IsZero()):
		return fmt.Errorf("%w: a fixed range needs both a start and an end", ErrValidation)
	case hasRange && !from.Before(to):
		return fmt.Errorf("%w: the start must be before the end", ErrValidation)
	case !expires.IsZero() && !expires.After(now):
		return fmt.Errorf("%w: the expiry must be in the future", ErrValidation)
	}
	return nil
}
