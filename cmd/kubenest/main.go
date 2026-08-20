package main

import (
	"os"

	"kubenest.io/cli/pkg/cmd"
)

func main() {
	if err := cmd.NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
