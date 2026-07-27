package worker

import (
	"log/slog"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/metadata"
	"github.com/RequiDev/encore/internal/spotify"
)

// MetadataChain builds the catalogue source enrichment reads from.
//
// With no fallback configured it is a pass-through to the Spotify client, so the
// enrichment worker has one code path rather than a branch at every fetch.
//
// A misconfigured fallback is a startup error rather than a warning. The whole
// point of the feature is to fill in metadata that would otherwise be blank, and
// a fallback that silently does nothing produces exactly the symptom it was
// turned on to cure — with the operator believing it is handled.
func MetadataChain(
	cfg config.MetadataFallback,
	client *spotify.Client,
	lg *slog.Logger,
) (*metadata.Chain, error) {
	if lg == nil {
		lg = slog.Default()
	}
	if !cfg.Enabled() {
		return metadata.NewChain(client, nil, metadata.WithChainLogger(lg)), nil
	}

	mirror, err := metadata.NewMirror(cfg, metadata.WithLogger(lg))
	if err != nil {
		return nil, err
	}

	note := "consulted while Spotify is rate limiting this instance, and for ids Spotify does not serve"
	if cfg.Prefer {
		note = "asked before Spotify, which is then only asked for what the fallback lacks"
	}
	lg.Info("a metadata fallback is configured",
		"url", cfg.URL,
		"authenticated", cfg.Token != "",
		"preferred", cfg.Prefer,
		"note", note)

	return metadata.NewChain(client, mirror,
		metadata.WithChainLogger(lg),
		// The limiter's pause is the signal that Spotify is holding the instance
		// back. Reading it here rather than discovering it from an error matters:
		// a paused limiter blocks rather than failing, and for an exhausted daily
		// quota it would block for most of a day before the fallback ever ran.
		metadata.WithPauseCheck(client.Limiter().PausedUntil),
		metadata.WithPreferredFallback(cfg.Prefer),
	), nil
}
