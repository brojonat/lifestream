// Command lifestream is Personal event store with an agent that files what you send it.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	root := &cli.Command{
		Name:    "lifestream",
		Usage:   "Personal event store with an agent that files what you send it",
		Version: version,
		Suggest: true,
		Commands: []*cli.Command{
			serveCommand(),
		},
	}
	if err := root.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "lifestream:", err)
		os.Exit(1)
	}
}
