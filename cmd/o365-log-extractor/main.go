// Command o365-log-extractor reads Office 365 unified audit records - either
// from the Management Activity API or from an on-disk export - normalises
// them and ships them to VictoriaLogs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/audit"
	"github.com/alex-j-butler/o365-log-extractor/internal/config"
	"github.com/alex-j-butler/o365-log-extractor/internal/o365"
	"github.com/alex-j-butler/o365-log-extractor/internal/state"
	"github.com/alex-j-butler/o365-log-extractor/internal/victorialogs"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	log := newLogger(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Info("shutting down")
			return
		}
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	// Logs go to stderr so that -dry-run output on stdout stays pipeable.
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if cfg.LogJSON {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	cfg.VL.Logger = log
	sink, err := victorialogs.New(cfg.VL)
	if err != nil {
		return err
	}

	if cfg.VL.DryRun {
		log.Info("starting", "version", version, "mode", cfg.Mode, "sink", "stdout (dry run)")
	} else {
		log.Info("starting", "version", version, "mode", cfg.Mode, "sink", sink.Endpoint())
	}

	switch cfg.Mode {
	case config.ModeFile:
		err = runFile(ctx, cfg, sink, log)
	case config.ModeAPI:
		err = runAPI(ctx, cfg, sink, log)
	default:
		err = fmt.Errorf("unsupported mode %q", cfg.Mode)
	}

	stats := sink.Stats()
	log.Info("finished", "records", stats.Records, "batches", stats.Batches, "bytes", stats.Bytes)
	return err
}

// runFile parses each named export and ships its records.
func runFile(ctx context.Context, cfg *config.Config, sink *victorialogs.Client, log *slog.Logger) error {
	for _, path := range cfg.Files {
		normalizer := audit.NewNormalizer()
		normalizer.Extra = map[string]string{"source": sourceName(path)}

		var parsed, skipped int
		err := audit.ReadFile(path, func(raw map[string]any) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, err := normalizer.Normalize(raw)
			if err != nil {
				skipped++
				log.Debug("skipping record", "file", path, "error", err)
				return nil
			}
			parsed++
			return sink.Add(ctx, record)
		})
		if err != nil {
			return err
		}
		log.Info("parsed file", "file", path, "records", parsed, "skipped", skipped)
	}
	return sink.Flush(ctx)
}

func sourceName(path string) string {
	if path == "-" {
		return "stdin"
	}
	return filepath.Base(path)
}

// runAPI polls the Management Activity API, ingesting every content blob it
// has not already seen.
func runAPI(ctx context.Context, cfg *config.Config, sink *victorialogs.Client, log *slog.Logger) error {
	client := o365.New(o365.Options{
		Cloud:        cfg.Cloud,
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		PublisherID:  cfg.PublisherID,
		Logger:       log,
		MaxRetries:   cfg.VL.MaxRetries,
	})

	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return err
	}

	if cfg.AutoSubscribe {
		if err := client.EnsureSubscriptions(ctx, cfg.ContentTypes); err != nil {
			return err
		}
	}

	for {
		if err := pollOnce(ctx, cfg, client, sink, st, log); err != nil {
			return err
		}
		if !cfg.Follow {
			return nil
		}
		log.Debug("sleeping until next poll", "interval", cfg.PollInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval):
		}
	}
}

// pollOnce reads one window of content for every configured content type.
func pollOnce(ctx context.Context, cfg *config.Config, client *o365.Client, sink *victorialogs.Client, st *state.State, log *slog.Logger) error {
	now := time.Now().UTC()

	for _, contentType := range cfg.ContentTypes {
		if err := ctx.Err(); err != nil {
			return err
		}

		start := now.Add(-cfg.Lookback)
		if cursor, ok := st.Cursor(contentType); ok {
			// Re-read a little before the cursor: the API publishes blobs
			// with a lag, and already-seen content IDs are skipped anyway.
			if resumed := cursor.Add(-cfg.Overlap); resumed.After(start) {
				start = resumed
			}
		}

		blobs, err := client.ListContent(ctx, contentType, start, now)
		if err != nil {
			return fmt.Errorf("list content %s: %w", contentType, err)
		}
		log.Debug("listed content", "content_type", contentType, "blobs", len(blobs), "start", start, "end", now)

		ingested, skipped := 0, 0
		for _, blob := range blobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if st.Seen(blob.ContentID) {
				skipped++
				continue
			}

			normalizer := audit.NewNormalizer()
			normalizer.Extra = map[string]string{
				"source":       "management-activity-api",
				"ContentType":  blob.ContentType,
				"TenantIdHint": cfg.TenantID,
			}

			records := 0
			err := client.FetchBlob(ctx, blob.ContentURI, func(raw map[string]any) error {
				record, normErr := normalizer.Normalize(raw)
				if normErr != nil {
					log.Debug("skipping record", "content_id", blob.ContentID, "error", normErr)
					return nil
				}
				records++
				return sink.Add(ctx, record)
			})
			if err != nil {
				// Flush what succeeded so a later failure does not discard
				// already-parsed records, then report the failure. The blob
				// is not marked seen, so the next poll retries it.
				if flushErr := sink.Flush(ctx); flushErr != nil {
					log.Error("flush after blob failure", "error", flushErr)
				}
				return fmt.Errorf("fetch blob %s: %w", blob.ContentID, err)
			}

			st.MarkSeen(blob.ContentID, blob.Expiration())
			ingested += records
		}

		// Flush before advancing the cursor so the cursor never claims
		// progress for records still sitting in the buffer.
		if err := sink.Flush(ctx); err != nil {
			return err
		}
		st.SetCursor(contentType, now)
		log.Info("polled content type", "content_type", contentType, "records", ingested, "blobs", len(blobs), "already_seen", skipped)
	}

	if removed := st.Prune(now); removed > 0 {
		log.Debug("pruned expired state entries", "count", removed)
	}
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
