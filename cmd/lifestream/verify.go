package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urfave/cli/v3"
)

func verifyCommand() *cli.Command {
	return &cli.Command{
		Name:  "verify",
		Usage: "check every owner's hash chain; prints one JSON line per owner",
		Description: `Exits non-zero if any chain is broken. The error names the first
event whose stored hashes don't match its contents.`,
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
			broken := 0
			for _, u := range users {
				line := map[string]any{"owner": u.ID, "ok": true}
				if err := st.Verify(ctx, u.ID); err != nil {
					line["ok"], line["error"] = false, err.Error()
					broken++
				}
				if err := enc.Encode(line); err != nil {
					return err
				}
			}
			if broken > 0 {
				return fmt.Errorf("%d broken chain(s)", broken)
			}
			return nil
		},
	}
}
