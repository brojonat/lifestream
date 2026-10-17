package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brojonat/lifestream/internal/store"
	"github.com/urfave/cli/v3"
)

func dataDirFlag() cli.Flag {
	return &cli.StringFlag{
		Name:        "data-dir",
		Usage:       "where lifestream.db and the bus live `DIR`",
		Sources:     cli.EnvVars("LIFESTREAM_DATA_DIR"),
		DefaultText: "~/.lifestream",
	}
}

func dataDir(cmd *cli.Command) (string, error) {
	if d := cmd.String("data-dir"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".lifestream"), nil
}

// openStore opens the event store under --data-dir, creating the directory.
func openStore(ctx context.Context, cmd *cli.Command) (*store.Store, string, error) {
	dir, err := dataDir(cmd)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("creating data dir: %w", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "lifestream.db"))
	if err != nil {
		return nil, "", err
	}
	return st, dir, nil
}
