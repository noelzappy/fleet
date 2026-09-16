package main

import (
	"os"

	"github.com/noelzappy/fleet/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		os.Exit(1)
	}
}
