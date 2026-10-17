package main

import (
	"context"
	"encoding/json"

	"github.com/urfave/cli/v3"
)

func usersCommand() *cli.Command {
	return &cli.Command{
		Name:  "users",
		Usage: "manage the allowlist of owners",
		Commands: []*cli.Command{
			{
				Name:  "add",
				Usage: "allowlist a phone number; prints the user and its API token as JSON",
				Description: `The token is shown once; only its hash is stored. Use it as
LIFESTREAM_TOKEN for push and the capture adapters.`,
				Flags: []cli.Flag{
					dataDirFlag(),
					&cli.StringFlag{Name: "name", Required: true, Usage: "display `NAME`"},
					&cli.StringFlag{Name: "phone", Required: true, Usage: "E.164 `NUMBER` texts come from, e.g. +15555550100"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					st, _, err := openStore(ctx, cmd)
					if err != nil {
						return err
					}
					defer st.Close()
					u, token, err := st.AddUser(ctx, cmd.String("name"), cmd.String("phone"))
					if err != nil {
						return err
					}
					return json.NewEncoder(cmd.Root().Writer).Encode(map[string]any{
						"id": u.ID, "name": u.Name, "phone": u.Phone, "token": token,
					})
				},
			},
			{
				Name:  "ls",
				Usage: "list users as JSONL",
				Flags: []cli.Flag{dataDirFlag()},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					st, _, err := openStore(ctx, cmd)
					if err != nil {
						return err
					}
					defer st.Close()
					users, err := st.Users(ctx)
					if err != nil {
						return err
					}
					enc := json.NewEncoder(cmd.Root().Writer)
					for _, u := range users {
						if err := enc.Encode(u); err != nil {
							return err
						}
					}
					return nil
				},
			},
		},
	}
}
