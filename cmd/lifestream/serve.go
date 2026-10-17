package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
	"github.com/brojonat/lifestream/internal/metrics"
	"github.com/brojonat/lifestream/internal/state"
	"github.com/brojonat/lifestream/internal/watch"
	"github.com/brojonat/lifestream/internal/web"
	"github.com/urfave/cli/v3"
)

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the app and open it in a browser",
		Description: `Starts an HTTP server bound to --addr with an embedded NATS JetStream
instance for state under --data-dir. Pages hold one long-lived SSE connection
each and repaint whenever state changes. The server shuts down cleanly on
SIGINT or SIGTERM. Log lines go to stderr; set LOG_LEVEL=debug|info|warn|error
and LOG_FORMAT=json for machine-readable logs.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Value:   "127.0.0.1:8080",
				Usage:   "listen `ADDR`",
				Sources: cli.EnvVars("LIFESTREAM_ADDR"),
			},
			&cli.StringFlag{
				Name:        "data-dir",
				Usage:       "where the embedded store keeps its files `DIR`",
				Sources:     cli.EnvVars("LIFESTREAM_DATA_DIR"),
				DefaultText: "~/.lifestream",
			},
			&cli.StringSliceFlag{
				Name:  "watch",
				Usage: "repaint pages when files under `DIR` change (repeatable)",
			},
			&cli.DurationFlag{
				Name:  "poll",
				Value: 2 * time.Second,
				Usage: "how often to check watched directories",
			},
			&cli.DurationFlag{
				Name:  "metrics-interval",
				Value: 5 * time.Second,
				Usage: "how often to sample the app's own resource usage",
			},
		},
		Action: runServe,
	}
}

func runServe(ctx context.Context, cmd *cli.Command) error {
	dataDir := cmd.String("data-dir")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		dataDir = filepath.Join(home, ".lifestream")
	}
	log := newLogger()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b, err := bus.Start(dataDir)
	if err != nil {
		return fmt.Errorf("starting embedded nats: %w", err)
	}
	defer b.Close()
	kv, err := b.KV(ctx, "app")
	if err != nil {
		return err
	}
	store := state.New(kv)
	go watch.Run(ctx, cmd.StringSlice("watch"), store, cmd.Duration("poll"), log)

	collector := metrics.NewCollector()
	mstream, err := b.Stream(ctx, metrics.StreamName, []string{metrics.Subject}, 24*time.Hour)
	if err != nil {
		return err
	}
	mkv, err := b.KV(ctx, "metrics")
	if err != nil {
		return err
	}
	interval := cmd.Duration("metrics-interval")
	go metrics.NewSampler(collector, b.JS(), mstream, mkv, log).Run(ctx, interval)

	addr := cmd.String("addr")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: web.NewHandler(web.Deps{
			App: "Lifestream", Store: store, Log: log,
			Collector: collector, MetricsKV: mkv, MetricsStream: mstream, JS: b.JS(), DataDir: dataDir, Interval: interval,
		}),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would sever the SSE streams.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "url", "http://"+ln.Addr().String(), "data_dir", dataDir)

	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// newLogger builds a slog logger from LOG_LEVEL and LOG_FORMAT.
func newLogger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(os.Getenv("LOG_LEVEL")))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
