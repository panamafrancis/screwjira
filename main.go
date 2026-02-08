package main

import (
	"os"

	"github.com/fraud-zero/fuckjira/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
