package main

import (
	"fmt"
	"os"

	"dv/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		if !cli.IsSilentError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
