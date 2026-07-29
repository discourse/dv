package main

import (
	"dv/internal/cli"
	"log"
	"os"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := cli.Execute(); err != nil {
		if cli.IsSilentError(err) {
			os.Exit(1)
		}
		log.Fatal(err)
	}
}
