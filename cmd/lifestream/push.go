package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/ingest"
	"github.com/urfave/cli/v3"
)

func pushCommand() *cli.Command {
	return &cli.Command{
		Name:      "push",
		Usage:     "POST events read from stdin to the service; prints the result JSON",
		UsageText: `echo '{"type":"note","payload":{"text":"hi"}}' | lifestream push`,
		Description: `Stdin is one JSON event, a JSON array, or JSONL. Each event is
{type, payload, occurred_at?, idempotency_key?, source?}; source defaults
to "cli".`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "url", Sources: cli.EnvVars("LIFESTREAM_URL"), Required: true, Usage: "service base `URL`"},
			&cli.StringFlag{Name: "token", Sources: cli.EnvVars("LIFESTREAM_TOKEN"), Required: true, Usage: "API `TOKEN` from users add"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			inputs, err := ingest.DecodeInputs(os.Stdin)
			if err != nil {
				return fmt.Errorf("reading stdin: %w", err)
			}
			for i := range inputs {
				if inputs[i].Source == "" {
					inputs[i].Source = "cli"
				}
			}
			body, err := json.Marshal(inputs)
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(cmd.String("url"), "/")+"/events", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cmd.String("token"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			if err != nil {
				return fmt.Errorf("posting events: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("posting events: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
			}
			_, err = io.Copy(cmd.Root().Writer, resp.Body)
			return err
		},
	}
}
