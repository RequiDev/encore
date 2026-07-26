// Package logging configures Encore's structured logger and carries a
// request-scoped logger through context.
//
// Every log record is structured (JSON in production, human-readable text in
// development). Values that could identify a listener or authenticate as one —
// tokens, cookies, session ids — must never be logged; use Redact for anything
// that might be sensitive.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configure the root logger.
type Options struct {
	Level  string
	Format string
	Source bool
	// Service names the process ("api", "worker", "migrate") and is attached to
	// every record so a single log stream can be split by producer.
	Service string
	Version string
	Out     io.Writer
}

// New builds the root logger.
func New(o Options) *slog.Logger {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	hopts := &slog.HandlerOptions{
		Level:       parseLevel(o.Level),
		AddSource:   o.Source,
		ReplaceAttr: replaceAttr,
	}

	var h slog.Handler
	if strings.EqualFold(o.Format, "text") {
		h = slog.NewTextHandler(out, hopts)
	} else {
		h = slog.NewJSONHandler(out, hopts)
	}

	attrs := []slog.Attr{}
	if o.Service != "" {
		attrs = append(attrs, slog.String("service", o.Service))
	}
	if o.Version != "" {
		attrs = append(attrs, slog.String("version", o.Version))
	}
	if len(attrs) > 0 {
		h = h.WithAttrs(attrs)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// replaceAttr shortens the source path and renames slog's defaults to the
// conventional lowercase field names.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "ts"
	case slog.MessageKey:
		a.Key = "msg"
	case slog.LevelKey:
		a.Key = "level"
	case slog.SourceKey:
		if src, ok := a.Value.Any().(*slog.Source); ok {
			a.Value = slog.StringValue(trimPath(src.File) + ":" + itoa(src.Line))
		}
	}
	return a
}

func trimPath(p string) string {
	// Keep the last two path elements: "internal/importer/runner.go".
	slash := strings.LastIndexByte(p, '/')
	if slash < 0 {
		slash = strings.LastIndexByte(p, '\\')
	}
	if slash < 0 {
		return p
	}
	prev := strings.LastIndexAny(p[:slash], "/\\")
	if prev < 0 {
		return p[slash+1:]
	}
	return strings.ReplaceAll(p[prev+1:], "\\", "/")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type ctxKey struct{}

// WithLogger returns a context carrying lg.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, lg)
}

// FromContext returns the request-scoped logger, or the default logger when the
// context carries none. It never returns nil.
func FromContext(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}

// Redact replaces all but the last four characters of a value. Use it for
// anything that identifies a credential; prefer omitting the value entirely.
func Redact(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", 8) + s[len(s)-4:]
}

// Err is the conventional attribute for an error.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", err.Error())
}
