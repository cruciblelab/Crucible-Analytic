package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// CategoryKey is the attribute that routes a record to a file.
//
//	logger.Info("login refused", logging.In(logging.CategoryAuth), ...)
//
// A record without it goes to app.log, so an ordinary log line written
// by code that knows nothing about this package still lands somewhere
// sensible.
const CategoryKey = "log_category"

// In returns the attribute that files a record under a category.
func In(c Category) slog.Attr { return slog.String(CategoryKey, string(c)) }

// Handler is an slog.Handler that writes into a Tree.
//
// It never returns an error to the caller for a write failure, and that
// is deliberate rather than sloppy. A service whose request handling
// fails because a log file could not be opened has turned its
// observability into an availability risk - exactly backwards. Failures
// go to the fallback writer (stderr by default) and the service carries
// on.
type Handler struct {
	tree     *Tree
	level    slog.Leveler
	fallback io.Writer

	attrs  []slog.Attr
	groups []string

	// handlers are the per-category JSON encoders, created on first use
	// and shared by every Handler derived from this one - so WithAttrs
	// does not multiply open files.
	mu       *sync.Mutex
	handlers map[Category]slog.Handler
}

// HandlerConfig configures a Handler.
type HandlerConfig struct {
	// Level is the minimum level written. Nil means slog.LevelInfo.
	Level slog.Leveler
	// Fallback receives records that could not be written to disk. Nil
	// means os.Stderr.
	Fallback io.Writer
}

// NewHandler wires a Handler over a Tree.
func NewHandler(tree *Tree, cfg HandlerConfig) *Handler {
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}
	if cfg.Fallback == nil {
		cfg.Fallback = os.Stderr
	}
	return &Handler{
		tree:     tree,
		level:    cfg.Level,
		fallback: cfg.Fallback,
		mu:       &sync.Mutex{},
		handlers: make(map[Category]slog.Handler, len(categories)),
	}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := h.clone()
	out.attrs = append(out.attrs, attrs...)
	return out
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := h.clone()
	out.groups = append(out.groups, name)
	return out
}

func (h *Handler) clone() *Handler {
	out := *h
	out.attrs = append([]slog.Attr(nil), h.attrs...)
	out.groups = append([]string(nil), h.groups...)
	return &out
}

// Handle writes one record to its category file, and mirrors anything at
// WARN or above into error.log as well.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	category, record := h.split(r)

	if err := h.writeTo(ctx, category, record); err != nil {
		h.reportFallback(category, record, err)
		return nil
	}

	// The mirror is what makes "did anything go wrong today" one file
	// rather than a search across nine. A record already in error.log is
	// not copied to itself.
	if r.Level >= slog.LevelWarn && category != CategoryError {
		if err := h.writeTo(ctx, CategoryError, record); err != nil {
			h.reportFallback(CategoryError, record, err)
		}
	}
	return nil
}

// split reads the routing attribute out of a record and returns the
// record with this handler's own attributes and groups applied.
func (h *Handler) split(r slog.Record) (Category, slog.Record) {
	category := CategoryApp

	own := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == CategoryKey {
			if c := Category(a.Value.String()); c.Valid() {
				category = c
			}
			// Dropped from the payload: it is routing information, and
			// repeating it inside every line of a file named after it is
			// noise.
			return true
		}
		own = append(own, a)
		return true
	})

	// Apply WithGroup nesting from the innermost outwards.
	for i := len(h.groups) - 1; i >= 0; i-- {
		own = []slog.Attr{{Key: h.groups[i], Value: slog.GroupValue(own...)}}
	}

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(h.attrs...)
	out.AddAttrs(own...)
	return category, out
}

func (h *Handler) writeTo(ctx context.Context, category Category, r slog.Record) error {
	h.mu.Lock()
	inner, ok := h.handlers[category]
	if !ok {
		inner = slog.NewJSONHandler(&categoryWriter{tree: h.tree, category: category}, &slog.HandlerOptions{
			Level:       h.level,
			ReplaceAttr: sanitizeAttr,
		})
		h.handlers[category] = inner
	}
	h.mu.Unlock()

	return inner.Handle(ctx, r)
}

// reportFallback is the last resort when disk writing fails. It must not
// itself fail loudly, so its own error is discarded.
func (h *Handler) reportFallback(category Category, r slog.Record, cause error) {
	fmt.Fprintf(h.fallback, "logging: could not write to %s (%v): %s %s\n",
		category, cause, r.Level, SanitizeValue(r.Message))
}

// categoryWriter adapts Tree.Write to io.Writer for slog.JSONHandler,
// which emits exactly one complete record per Write call.
type categoryWriter struct {
	tree     *Tree
	category Category
}

func (w *categoryWriter) Write(p []byte) (int, error) {
	// JSONHandler terminates each record with a newline; Tree.Write adds
	// its own, so trim to avoid a blank line between every entry.
	if err := w.tree.Write(w.category, bytes.TrimRight(p, "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// sanitizeAttr is slog's per-attribute hook, and the single place every
// value passes through before it can reach a file.
func sanitizeAttr(_ []string, a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	// Resolve LogValuer first, or a lazily-computed secret would slip
	// past the check below.
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, SanitizeValue(a.Value.String()))
	}
	return a
}
