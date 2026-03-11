package main

import (
	"os"

	"github.com/AltScore/altscore-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
