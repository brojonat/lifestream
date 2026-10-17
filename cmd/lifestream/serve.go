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
	"strings"
	"syscall"
	"time"

	"github.com/brojonat/lifestream/internal/api"
	"github.com/brojonat/lifestream/internal/bus"
	"github.com/brojonat/lifestream/internal/dispatch"
	"github.com/brojonat/lifestream/internal/ingest"
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
				Name:    "api-addr",
				Value:   "127.0.0.1:8081",
				Usage:   "listen `ADDR` for the agent's internal API; never expose it",
				Sources: cli.EnvVars("LIFESTREAM_API_ADDR"),
			},
			dataDirFlag(),
			&cli.StringFlag{
				Name:    "agent-url",
				Usage:   "the agent's base `URL`; without it raw events queue undelivered",
				Sources: cli.EnvVars("LIFESTREAM_AGENT_URL"),
			},
			&cli.StringFlag{
				Name:    "public-url",
				Usage:   "base `URL` as Twilio sees it, for webhook signatures",
				Sources: cli.EnvVars("LIFESTREAM_PUBLIC_URL"),
			},
			&cli.StringFlag{
				Name:    "twilio-auth-token",
				Usage:   "Twilio auth `TOKEN`; /twilio/sms is off without it",
				Sources: cli.EnvVars("TWILIO_AUTH_TOKEN"),
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
	log := newLogger()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, dataDir, err := openStore(ctx, cmd)
	if err != nil {
		return err
	}
	defer st.Close()

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

	if agentURL := cmd.String("agent-url"); agentURL != "" {
		d := dispatch.New(st, agentURL, &http.Client{Timeout: 30 * time.Second}, log, dispatch.Options{})
		go d.Run(ctx)
	} else {
		log.Warn("LIFESTREAM_AGENT_URL unset; events queue until an agent is configured")
	}

	addr := cmd.String("addr")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	if token := cmd.String("twilio-auth-token"); token != "" {
		if cmd.String("public-url") == "" {
			return fmt.Errorf("--public-url (LIFESTREAM_PUBLIC_URL) is required with a Twilio auth token")
		}
		mux.Handle("POST /twilio/sms", ingest.Twilio(st, token, cmd.String("public-url"), log))
	} else {
		log.Warn("TWILIO_AUTH_TOKEN unset; /twilio/sms is off")
	}
	mux.Handle("POST /events", ingest.Events(st, log))
	mux.Handle("/", web.NewHandler(web.Deps{
		App: "Lifestream", Store: store, Log: log,
		Collector: collector, MetricsKV: mkv, MetricsStream: mstream, JS: b.JS(), DataDir: dataDir, Interval: interval,
	}))
	servers := []*http.Server{newServer(ctx, mux), newServer(ctx, api.NewHandler(st, log))}
	apiAddr := cmd.String("api-addr")
	apiLn, err := net.Listen("tcp", apiAddr)
	if err != nil {
		ln.Close()
		return fmt.Errorf("listening on %s: %w", apiAddr, err)
	}

	errc := make(chan error, 2)
	go func() { errc <- servers[0].Serve(ln) }()
	go func() { errc <- servers[1].Serve(apiLn) }()
	log.Info("listening", "url", "http://"+ln.Addr().String(), "api", "http://"+apiLn.Addr().String(), "data_dir", dataDir)

	var serveErr error
	select {
	case err := <-errc:
		serveErr = fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, fmt.Errorf("shutdown: %w", err))
		}
	}
	return serveErr
}

func newServer(ctx context.Context, h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would sever the SSE streams.
	}
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
