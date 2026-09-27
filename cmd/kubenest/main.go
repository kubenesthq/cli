package main

import (
	"os"

	"kubenest.io/cli/pkg/cmd"
)

func main() {
	err := cmd.NewRootCommand().Execute()
	if code := cmd.ExitCode(err); code != 0 {
		os.Exit(code)
	}
}
