package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Config is the [logging] section every service shares.
type Config struct {
	// Dir is the single root every service's tree lives under. Empty
	// disables file logging entirely and leaves stderr as it was, which
	// is what a developer running the binary by hand wants.
	Dir string `toml:"dir"`
	// Level is "debug", "info", "warn" or "error". Empty means info.
	Level string `toml:"level"`
	// RetentionDays is how many day-directories to keep. Zero takes
	// DefaultRetentionDays; negative disables pruning.
	RetentionDays int `toml:"retention_days"`
	// MaxFileMB rotates a category file once it exceeds this. Zero takes
	// the default.
	MaxFileMB int `toml:"max_file_mb"`
	// AlsoStderr keeps writing to stderr alongside the tree. Useful
	// under systemd, where journald collects stderr anyway.
	AlsoStderr bool `toml:"also_stderr"`
}

// ParseLevel turns the configured name into a level.
func ParseLevel(name string) (slog.Level, error) {
	switch name {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: invalid level %q (want debug, info, warn or error)", name)
	}
}

// Setup builds the logger a service should use, and returns a cleanup
// function.
//
// When Dir is empty this returns an ordinary stderr logger and a no-op
// cleanup, so nothing in a service's startup path has to branch on
// whether file logging is configured.
func Setup(service string, cfg Config) (*slog.Logger, func(), error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	if cfg.Dir == "" {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})), func() {}, nil
	}

	tree, err := NewTree(TreeConfig{
		Root:          cfg.Dir,
		Service:       service,
		RetentionDays: cfg.RetentionDays,
		MaxFileBytes:  int64(cfg.MaxFileMB) << 20,
	})
	if err != nil {
		return nil, nil, err
	}

	var handler slog.Handler = NewHandler(tree, HandlerConfig{Level: level})
	if cfg.AlsoStderr {
		handler = &teeHandler{
			primary: handler,
			// stderr gets the same records, so journald still shows what
			// the operator expects while the tree keeps the readable copy.
			secondary: slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level, ReplaceAttr: sanitizeAttr}),
		}
	}

	// Prune once at startup rather than on a timer. A service that runs
	// for months rolls day directories as it goes, and one that restarts
	// daily prunes daily; a background goroutine would buy the remaining
	// case at the cost of a deletion racing a write.
	if _, err := tree.Prune(); err != nil {
		// Not fatal. A tree that cannot be pruned still logs, and
		// refusing to start over stale directories would turn a disk
		// housekeeping problem into an outage.
		fmt.Fprintf(os.Stderr, "logging: prune failed: %v\n", err)
	}

	return slog.New(handler), func() { _ = tree.Close() }, nil
}

// teeHandler writes each record to two handlers.
type teeHandler struct {
	primary, secondary slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.primary.Enabled(ctx, l) || t.secondary.Enabled(ctx, l)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	// The primary's own failures are already handled by falling back, so
	// neither error should stop the other from being written.
	_ = t.primary.Handle(ctx, r.Clone())
	_ = t.secondary.Handle(ctx, r)
	return nil
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{primary: t.primary.WithAttrs(attrs), secondary: t.secondary.WithAttrs(attrs)}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{primary: t.primary.WithGroup(name), secondary: t.secondary.WithGroup(name)}
}

// Deadline is a convenience for logging how long something took, used by
// the query and access categories.
func Deadline(started time.Time) slog.Attr {
	return slog.Duration("took", time.Since(started))
}
