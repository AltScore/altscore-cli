package main

import (
	"errors"
	"os"

	"github.com/AltScore/altscore-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		var ec *cmd.ExitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		os.Exit(1)
	}
}
