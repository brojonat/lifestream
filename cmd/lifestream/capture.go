package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/capture"
	"github.com/urfave/cli/v3"
)

func captureCommand() *cli.Command {
	names := make([]string, 0)
	for name := range capture.Harnesses() {
		names = append(names, name)
	}
	slices.Sort(names)
	return &cli.Command{
		Name:      "capture",
		Usage:     "Send a harness transcript's new lines as llm.message events (run from the harness's hook)",
		UsageText: "lifestream capture <harness> < hook.json    # harnesses: " + strings.Join(names, ", "),
		Description: `Reads the hook's JSON on stdin, then the transcript from the offset saved
for the session, sends the new complete lines, and saves the new offset
once the service has them. Does nothing unless LIFESTREAM_CAPTURE=on, and
nothing for a cwd under a LIFESTREAM_DENY glob (colon-separated, ~ allowed).

Prints nothing on stdout (hooks read it). If the service can't be reached
it warns on stderr and exits 0: the next turn sends everything since the
last success.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "url", Sources: cli.EnvVars("LIFESTREAM_URL"), Usage: "service base `URL`"},
			&cli.StringFlag{Name: "token", Sources: cli.EnvVars("LIFESTREAM_TOKEN"), Usage: "API `TOKEN` from users add"},
			&cli.StringFlag{Name: "capture", Sources: cli.EnvVars("LIFESTREAM_CAPTURE"), Usage: "`on` to capture; anything else captures nothing"},
			&cli.StringFlag{Name: "deny", Sources: cli.EnvVars("LIFESTREAM_DENY"), Usage: "colon-separated path `GLOBS` never to capture"},
			&cli.StringFlag{Name: "state-dir", Usage: "where offsets live (default $XDG_STATE_HOME/lifestream/capture)"},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Second, Usage: "per request; hooks wait for this command"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			h, ok := capture.Harnesses()[cmd.Args().First()]
			if !ok {
				return fmt.Errorf("capture: want one harness argument, one of %s", strings.Join(names, ", "))
			}
			stdin, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			if err != nil {
				return fmt.Errorf("capture: reading stdin: %w", err)
			}
			stateDir := cmd.String("state-dir")
			if stateDir == "" {
				if stateDir, err = defaultStateDir(); err != nil {
					return err
				}
			}
			cfg := capture.Config{
				URL:      cmd.String("url"),
				Token:    cmd.String("token"),
				On:       cmd.String("capture") == "on",
				StateDir: stateDir,
				Client:   &http.Client{Timeout: cmd.Duration("timeout")},
			}
			if d := cmd.String("deny"); d != "" {
				cfg.Deny = strings.Split(d, ":")
			}
			_, err = capture.Run(ctx, cfg, h, stdin)
			var te *capture.TransportError
			if errors.As(err, &te) {
				fmt.Fprintln(os.Stderr, "lifestream capture: will retry next turn:", err)
				return nil
			}
			if err != nil {
				return fmt.Errorf("capture %s: %w", h.Name, err)
			}
			return nil
		},
	}
}

func defaultStateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("capture: finding the state dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "lifestream", "capture"), nil
}
