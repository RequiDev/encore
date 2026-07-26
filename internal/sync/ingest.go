package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/store/listens"
)

// batch is one poll's worth of work, already converted, validated and ready to
// be written.
type batch struct {
	// staged are the listens to insert.
	staged []listens.StagedListen
	// trackIDs are the catalogue ids the staged listens reference. Every one of
	// them must exist before the insert, because listens.track_id is a foreign
	// key.
	trackIDs []string
	// tracks and albums are the catalogue detail this page carried for free.
	tracks []domain.Track
	albums []domain.Album
	// newest is the newest play the batch accounted for, and the value the
	// cursor may advance to once the batch commits.
	newest time.Time
	// skipped and invalid count the entries that produced no listen.
	skipped int
	invalid int
}

// prepare converts a page of play history into insertable rows.
//
// It is a pure function of its inputs — no store, no clock beyond the now it is
// handed — so the decisions that matter here (what a listen's timestamp,
// precision and duration are, and which entries move the watermark) are
// testable without a database or a network.
func prepare(userID uuid.UUID, plays []spotify.PlayHistory, now time.Time) batch {
	b := batch{staged: make([]listens.StagedListen, 0, len(plays))}
	seenTrack := make(map[string]struct{}, len(plays))
	seenAlbum := make(map[string]struct{}, len(plays))

	for _, play := range plays {
		if !play.Track.IsMusic() {
			// Podcast episodes, audiobook chapters and local files share this
			// feed with music and have no catalogue identity Encore can store.
			b.skipped++
			// The watermark still moves past them; otherwise a listener whose
			// most recent play is a podcast would have it re-fetched for ever.
			if plausible(play.PlayedAt, now) {
				b.newest = later(b.newest, play.PlayedAt.UTC())
			}
			continue
		}

		l := listenFrom(userID, play)
		if err := l.Validate(now); err != nil {
			// Counted, not fatal. One entry with a corrupt timestamp must not
			// cost the listener the rest of the page, and it must not drag the
			// watermark past plays that have not been read yet.
			b.invalid++
			continue
		}

		b.newest = later(b.newest, l.PlayedAt)
		b.staged = append(b.staged, listens.Stage(l, nil))

		if _, dup := seenTrack[play.Track.ID]; dup {
			continue
		}
		seenTrack[play.Track.ID] = struct{}{}
		b.trackIDs = append(b.trackIDs, play.Track.ID)

		// A play-history entry carries the full track object, so the catalogue
		// detail is already paid for; making the enrichment workers fetch it
		// again would spend the rate limit to learn what is in hand. A track
		// object without a name is not the full object, so it is left pending
		// instead of being recorded as resolved with nothing in it.
		if play.Track.Name == "" {
			continue
		}
		b.tracks = append(b.tracks, play.Track.ToDomainTrack())

		album := play.Track.Album
		if album.ID == "" || album.Name == "" {
			continue
		}
		if _, dup := seenAlbum[album.ID]; dup {
			continue
		}
		seenAlbum[album.ID] = struct{}{}
		b.albums = append(b.albums, album.ToDomainAlbum())
	}
	return b
}

// listenFrom converts one play-history entry into a domain listen.
//
// PlayedAt is used exactly as Spotify reported it. This is the one source that
// timestamps the *start* of playback, so nothing has to be derived from a
// duration, and the precision is milliseconds, which is what keeps the
// cross-source duplicate window at ten seconds rather than sixty.
//
// ms_played is the track's own duration, because the feed does not report how
// much of the track was actually heard. It is an estimate and it is the only one
// available: recording zero would understate a live-synced listener's time by
// the whole of their history. The consequence is worth stating plainly — where a
// play is later also present in an extended-history export, which does carry the
// real figure, the cross-source rule keeps the row that arrived first, so the
// estimate stands.
func listenFrom(userID uuid.UUID, play spotify.PlayHistory) domain.Listen {
	return domain.Listen{
		UserID:    userID,
		PlayedAt:  play.PlayedAt.UTC(),
		Precision: domain.PrecisionMillisecond,
		Identity:  domain.TrackIdentityFromID(play.Track.ID),
		MsPlayed:  msPlayed(play.Track),
		Source:    domain.SourceSync,
	}
}

// msPlayed narrows a track duration to what a listen may carry, so an absurd
// value from upstream is clamped rather than rejected by the database.
func msPlayed(t spotify.Track) int32 {
	switch {
	case t.DurationMs <= 0:
		return 0
	case int64(t.DurationMs) > int64(domain.MaxMsPlayed):
		return domain.MaxMsPlayed
	default:
		return int32(t.DurationMs)
	}
}

// commit writes one batch and advances the cursor, and reports how many listens
// were new.
//
// The insert and the cursor update are in the same transaction. That is the
// whole durability story of the poller: if the process dies between them, the
// next poll asks for the same window again and the duplicate rules make it a
// no-op, whereas advancing the cursor first would lose those plays permanently,
// because Spotify only ever returns the last fifty.
func (p *Poller) commit(ctx context.Context, userID uuid.UUID, b batch, timezone string) (int64, error) {
	if len(b.staged) == 0 {
		// Nothing to write, but the poll still happened, and a page of nothing
		// but podcasts still moves the watermark past them.
		if err := p.dep.Accounts.Credentials.MarkSyncResult(
			ctx, p.dep.Store.DB(), userID, cursorPtr(b.newest), nil); err != nil {
			return 0, fmt.Errorf("record sync result: %w", err)
		}
		return 0, nil
	}

	var inserted int64
	err := p.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Track rows first: listens.track_id is a foreign key, and the pending
		// row is what keeps ingestion independent of the catalogue. It is
		// created even for a track whose detail is being upserted immediately
		// below, so the key holds whatever the upsert makes of the payload.
		if err := p.dep.Listens.EnsureTracks(ctx, tx, b.trackIDs); err != nil {
			return err
		}
		if err := p.dep.Catalog.UpsertTracks(ctx, tx, b.tracks); err != nil {
			return err
		}
		if err := p.dep.Catalog.UpsertAlbums(ctx, tx, b.albums); err != nil {
			return err
		}
		// The credit links have to be written here as well. Upserting a track as
		// resolved takes it out of the enrichment queue, so nothing else would
		// ever fill them in, and without them a listen cannot be attributed to
		// an artist. Both statements are bounded by the fifty plays a page can
		// hold, and only run when there were new plays at all.
		//
		// The artists themselves are deliberately *not* upserted: the objects
		// embedded in a track are the simplified form, with no genres,
		// followers or images, so recording them as resolved would take them
		// out of the queue having learned almost nothing. They are created as
		// pending rows by these statements and filled in by internal/enrich.
		for _, t := range b.tracks {
			if err := p.dep.Catalog.ReplaceTrackArtists(ctx, tx, t.ID, t.ArtistIDs); err != nil {
				return err
			}
		}
		for _, a := range b.albums {
			if err := p.dep.Catalog.ReplaceAlbumArtists(ctx, tx, a.ID, a.ArtistIDs); err != nil {
				return err
			}
		}

		n, err := p.dep.Listens.InsertListens(ctx, tx, b.staged, timezone)
		if err != nil {
			return err
		}
		inserted = n

		// Same transaction as the rows it describes. MarkSyncResult only ever
		// moves the watermark forward, so a poll that raced with a wider one
		// cannot rewind it.
		return p.dep.Accounts.Credentials.MarkSyncResult(ctx, tx, userID, cursorPtr(b.newest), nil)
	})
	if err != nil {
		return 0, fmt.Errorf("commit sync batch: %w", err)
	}
	return inserted, nil
}

// cursorPtr renders the watermark for the store, which reads nil as "leave the
// cursor where it is".
func cursorPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// plausible reports whether a timestamp is sane enough to move the watermark.
//
// It is the same window domain.Listen.Validate applies, so an entry Encore does
// not store is held to the same standard as one it does: a corrupt date can
// never push the cursor past plays that have not been read yet.
func plausible(t, now time.Time) bool {
	return !t.IsZero() &&
		!t.Before(domain.EarliestPlausibleListen) &&
		!t.After(now.Add(domain.FutureSkew))
}

// later returns whichever instant is the more recent.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
