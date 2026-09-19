package main

import (
	"os"

	"github.com/noelzappy/fleet/internal/cli"
	"github.com/noelzappy/fleet/internal/ui"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		ui.Fail(err)
		os.Exit(1)
	}
}
