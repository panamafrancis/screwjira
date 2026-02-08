package cmd

import (
	"fmt"
	"strings"

	"github.com/fraud-zero/fuckjira/internal/jira"
	"github.com/fraud-zero/fuckjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	ingestProject string
	ingestAll     bool
	ingestStatus  bool
	ingestLimit   int
)

// Project configurations
var projects = map[string]struct {
	Name      string
	JQL       string
	ExcludeStatuses []string
}{
	"F0": {
		Name:      "Product Development",
		JQL:       `project = "F0" AND status NOT IN ("Done", "Won't Do") ORDER BY created ASC`,
		ExcludeStatuses: []string{"Done", "Won't Do"},
	},
	"DISC": {
		Name:      "Discovery (JPD)",
		JQL:       `project = "DISC" AND status NOT IN ("Released", "Abandoned") ORDER BY created ASC`,
		ExcludeStatuses: []string{"Released", "Abandoned"},
	},
}

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Ingest issues from Jira",
	Long:  `Fetch issues from Jira/JPD and store them locally for filtering.`,
	RunE:  runIngest,
}

func init() {
	rootCmd.AddCommand(ingestCmd)
	ingestCmd.Flags().StringVarP(&ingestProject, "project", "p", "", "project to ingest (F0 or DISC)")
	ingestCmd.Flags().BoolVar(&ingestAll, "all", false, "ingest all projects")
	ingestCmd.Flags().BoolVar(&ingestStatus, "status", false, "show ingestion status")
	ingestCmd.Flags().IntVarP(&ingestLimit, "limit", "l", 0, "limit number of issues to fetch (0=all)")
}

func runIngest(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if ingestStatus {
		return showIngestStatus(store)
	}

	if !ingestAll && ingestProject == "" {
		return fmt.Errorf("specify --project or --all")
	}

	var projectsToIngest []string
	if ingestAll {
		for p := range projects {
			projectsToIngest = append(projectsToIngest, p)
		}
	} else {
		p := strings.ToUpper(ingestProject)
		if _, ok := projects[p]; !ok {
			return fmt.Errorf("unknown project: %s (valid: F0, DISC)", ingestProject)
		}
		projectsToIngest = []string{p}
	}

	for _, project := range projectsToIngest {
		if err := ingestProject_(store, project); err != nil {
			return err
		}
	}

	return showIngestStatus(store)
}

func ingestProject_(store *storage.Store, project string) error {
	cfg := projects[project]
	fmt.Printf("Ingesting %s (%s)...\n", project, cfg.Name)

	// First get count
	count, err := jira.Count(cfg.JQL)
	if err != nil {
		return fmt.Errorf("failed to count issues: %w", err)
	}
	fmt.Printf("  Found %d issues\n", count)

	// Fetch all issue keys
	limit := ingestLimit
	fmt.Printf("  Fetching issue list...")
	issues, err := jira.Search(cfg.JQL, limit)
	if err != nil {
		return fmt.Errorf("failed to search issues: %w", err)
	}
	fmt.Printf(" got %d\n", len(issues))

	// Fetch full details for each issue
	fmt.Printf("  Fetching full details:\n")
	for i, issue := range issues {
		fmt.Printf("\r  [%d/%d] %s - %s", i+1, len(issues), issue.Key, truncate(issue.Summary(), 50))

		fullIssue, err := jira.GetIssue(issue.Key)
		if err != nil {
			fmt.Printf("\n  Warning: failed to fetch %s: %v\n", issue.Key, err)
			continue
		}

		if err := store.SaveIssue(fullIssue); err != nil {
			fmt.Printf("\n  Warning: failed to save %s: %v\n", issue.Key, err)
			continue
		}
	}
	fmt.Println()

	return nil
}

func showIngestStatus(store *storage.Store) error {
	stats, err := store.Stats()
	if err != nil {
		return fmt.Errorf("failed to get stats: %w", err)
	}

	fmt.Println("\nIngestion Status:")
	fmt.Printf("  Total issues: %d\n", stats.Total)
	for project, count := range stats.ByProject {
		fmt.Printf("    %s: %d\n", project, count)
	}
	fmt.Println("\nFilter Status:")
	fmt.Printf("  Pending: %d\n", stats.Pending)
	fmt.Printf("  Keep:    %d\n", stats.Keep)
	fmt.Printf("  Skip:    %d\n", stats.Skip)
	fmt.Printf("  Defer:   %d\n", stats.Defer)

	return nil
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
