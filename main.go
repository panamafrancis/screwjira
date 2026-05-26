package main

import (
	"os"

	"github.com/panamafrancis/screwjira/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
