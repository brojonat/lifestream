package main

import (
	"context"

	"github.com/urfave/cli/v3"
)

func rebuildCommand() *cli.Command {
	return &cli.Command{
		Name:  "rebuild",
		Usage: "drop every projection and rebuild it from the event log",
		Description: `Collections, members and metric values are views over events. This
replays every event in order, in one transaction, so a failure leaves the
old projections in place. Safe to run while serve is up; appends wait.`,
		Flags: []cli.Flag{dataDirFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			st, _, err := openStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer st.Close()
			return st.Rebuild(ctx)
		},
	}
}
