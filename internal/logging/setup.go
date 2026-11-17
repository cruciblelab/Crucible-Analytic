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
	// RetentionDays is how long an ordinary category is kept. Zero takes
	// the default; negative disables deletion.
	RetentionDays int `toml:"retention_days"`
	// ImportantRetentionDays is how long security, auth and audit are
	// kept. They are small and are exactly what somebody asks for a year
	// later, so they get their own, much longer figure.
	ImportantRetentionDays int `toml:"important_retention_days"`
	// ArchiveAfterDays compresses a day's files once they are older than
	// this. Zero takes the default; negative disables compression.
	ArchiveAfterDays int `toml:"archive_after_days"`
	// MaxFileMB rotates a category file once it exceeds this. Zero takes
	// the default.
	MaxFileMB int `toml:"max_file_mb"`
	// AlsoStderr keeps writing to stderr alongside the tree. Useful
	// under systemd, where journald collects stderr anyway.
	AlsoStderr bool `toml:"also_stderr"`
}

// Lifecycle turns the configured numbers into a retention policy,
// filling in the defaults for anything left at zero.
func (c Config) Lifecycle() Lifecycle {
	policy := DefaultLifecycle()
	if c.RetentionDays != 0 {
		policy.RetentionDays = c.RetentionDays
	}
	if c.ImportantRetentionDays != 0 {
		policy.ImportantRetentionDays = c.ImportantRetentionDays
	}
	if c.ArchiveAfterDays != 0 {
		policy.ArchiveAfterDays = c.ArchiveAfterDays
	}
	return policy
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
func Setup(service string, cfg Config) (*slog.Logger, *Controls, func(), error) {
	configured, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, nil, nil, err
	}

	// A LevelVar rather than a fixed level: slog reads it on every
	// record, so a level change takes effect on the next line instead of
	// the next restart.
	level := &slog.LevelVar{}
	level.Set(configured)
	controls := &Controls{level: level, base: configured,
		keep: &keeper{file: cfg.Lifecycle()}}

	if cfg.Dir == "" {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})), controls, func() {}, nil
	}

	tree, err := NewTree(TreeConfig{
		Root:          cfg.Dir,
		Service:       service,
		RetentionDays: cfg.RetentionDays,
		MaxFileBytes:  int64(cfg.MaxFileMB) << 20,
	})
	if err != nil {
		return nil, nil, nil, err
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

	// No maintenance pass here. There was one, with the config file's
	// numbers, and it was wrong twice over.
	//
	// It was the only pass any process ever made - its comment said "the
	// panel's repair operation covers the long-running case", and nothing
	// did - so a service that ran for months kept months of logs.
	//
	// And it ran before the service could know the panel's policy. A
	// deployment whose owner had lengthened a retention in the panel lost,
	// on every restart, the files the panel said to keep: measured on the
	// real beacon, important retention 400 days in the panel, a 370-day-old
	// security log, gone at startup. A deletion cannot be taken back, and
	// these two figures carry legal weight.
	//
	// The first pass is now the caller's, through Controls.Keep: KeepLive
	// once the settings have been read, KeepFile for a service whose file
	// is the whole policy. See keep.go.
	controls.keep.tree = tree

	return slog.New(handler), controls, func() { _ = tree.Close() }, nil
}

// Tee returns a handler that writes each record to both.
//
// Exported because the panel log sink (internal/logsink) is the second
// destination a service's records go to, and it needs the same fan-out
// the AlsoStderr option has always used. One implementation rather than
// two: a second copy would be the one that stops matching.
func Tee(primary, secondary slog.Handler) slog.Handler {
	return &teeHandler{primary: primary, secondary: secondary}
}

// teeHandler writes each record to two handlers.
type teeHandler struct {
	primary, secondary slog.Handler
}

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.primary.Enabled(ctx, l) || t.secondary.Enabled(ctx, l)
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	// Each child is asked again, because the two can sit at different
	// levels. Until the panel sink existed both branches shared one
	// LevelVar and this was always true twice, so the checks cost
	// nothing then and are load-bearing now: the sink's floor is WARN
	// while the tree may be at INFO, and Enabled above is an OR - it
	// says *somebody* wants the record, not that both do.
	//
	// The primary's own failures are already handled by falling back, so
	// neither error should stop the other from being written.
	if t.primary.Enabled(ctx, r.Level) {
		_ = t.primary.Handle(ctx, r.Clone())
	}
	if t.secondary.Enabled(ctx, r.Level) {
		_ = t.secondary.Handle(ctx, r)
	}
	return nil
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{primary: t.primary.WithAttrs(attrs), secondary: t.secondary.WithAttrs(attrs)}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{primary: t.primary.WithGroup(name), secondary: t.secondary.WithGroup(name)}
}

// Deadline is a convenience for logging how long something took.
//
// Its comment used to say "used by the query and access categories". It
// was used by neither, and by nothing else - a deadcode scan is what
// said so. Kept because it is the right shape for the duration attribute
// and costs nothing, but described honestly: this is available, not
// established. A comment naming callers that do not exist teaches a
// reader to trust the next one less.
func Deadline(started time.Time) slog.Attr {
	return slog.Duration("took", time.Since(started))
}
