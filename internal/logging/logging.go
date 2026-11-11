// Package logging builds the JSON logger. Logs never carry tokens, issue or comment bodies.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger on stderr at the given level (debug, info, warn, error).
func New(level string) *slog.Logger {
	return NewTo(os.Stderr, level)
}

// NewTo writes to w.
func NewTo(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})).
		With("service", "jiku-github-sync")
}

// ParseLevel maps a level name to slog; anything unknown is info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
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

// Discard is a logger that drops everything, for tests.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
