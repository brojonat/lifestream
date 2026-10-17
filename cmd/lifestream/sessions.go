package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/brojonat/lifestream/internal/store"
	"github.com/urfave/cli/v3"
)

func sessionsCommand() *cli.Command {
	owner := &cli.StringFlag{Name: "owner", Usage: "only this user `ID` (default: every owner)"}
	return &cli.Command{
		Name:  "sessions",
		Usage: "read captured harness sessions in the common format",
		Description: `Sessions are the llm.message events that share a session_id. Every
harness's messages are read into one shape: {role, parts: [{kind, text,
name, call_id, input, error}]}, with role user, assistant, tool or system
and kind text, tool_call, tool_result, reasoning, image or other.`,
		Commands: []*cli.Command{
			{
				Name:  "ls",
				Usage: "list sessions as JSONL, most recently active first",
				Flags: []cli.Flag{dataDirFlag(), owner, &cli.IntFlag{Name: "limit", Value: 100, Usage: "at most `N` sessions"}},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					st, _, err := openStore(ctx, cmd)
					if err != nil {
						return err
					}
					defer st.Close()
					list, err := st.Sessions(ctx, cmd.String("owner"), int(cmd.Int("limit")))
					if err != nil {
						return err
					}
					enc := json.NewEncoder(cmd.Root().Writer)
					for _, s := range list {
						if err := enc.Encode(s); err != nil {
							return err
						}
					}
					return nil
				},
			},
			{
				Name:      "show",
				Usage:     "print one session's messages as JSONL, in sequence order",
				ArgsUsage: "<session-id>",
				Flags:     []cli.Flag{dataDirFlag(), owner},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() != 1 {
						return fmt.Errorf("sessions show: want one session id")
					}
					st, _, err := openStore(ctx, cmd)
					if err != nil {
						return err
					}
					defer st.Close()
					ms, err := st.SessionMessages(ctx, cmd.String("owner"), cmd.Args().First())
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("sessions show: no session %q", cmd.Args().First())
					}
					if err != nil {
						return fmt.Errorf("sessions show: %w", err)
					}
					enc := json.NewEncoder(cmd.Root().Writer)
					for _, m := range ms {
						if err := enc.Encode(m); err != nil {
							return err
						}
					}
					return nil
				},
			},
		},
	}
}
