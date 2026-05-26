package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	dataDir string
)

var rootCmd = &cobra.Command{
	Use:   "screwjira",
	Short: "Migrate Jira issues to GitHub Projects",
	Long:  `A CLI tool to export issues from Jira/JPD to GitHub Projects with filtering and enrichment.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "data directory (default: ~/.screwjira)")
}

func initConfig() {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error getting home directory:", err)
			os.Exit(1)
		}
		dataDir = filepath.Join(home, ".screwjira")
	}
}

func getDataDir() string {
	return dataDir
}
