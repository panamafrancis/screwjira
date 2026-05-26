package cmd

import (
	"fmt"
	"strings"

	"github.com/panamafrancis/screwjira/internal/config"
	"github.com/panamafrancis/screwjira/internal/jira"
	"github.com/panamafrancis/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	ingestProject string
	ingestAll     bool
	ingestStatus  bool
	ingestLimit   int
	ingestSync    bool
)


var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Ingest issues from Jira",
	Long:  `Fetch issues from Jira/JPD and store them locally for filtering.`,
	RunE:  runIngest,
}

func init() {
	rootCmd.AddCommand(ingestCmd)
	ingestCmd.Flags().StringVarP(&ingestProject, "project", "p", "", "Jira project key to ingest (e.g. ENG)")
	ingestCmd.Flags().BoolVar(&ingestAll, "all", false, "ingest all configured projects")
	ingestCmd.Flags().BoolVar(&ingestStatus, "status", false, "show ingestion status")
	ingestCmd.Flags().IntVarP(&ingestLimit, "limit", "l", 0, "limit number of issues to fetch (0=all)")
	ingestCmd.Flags().BoolVar(&ingestSync, "sync", false, "re-fetch all stored issues and auto-skip ones that are now closed")
}

func runIngest(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if ingestStatus {
		return showIngestStatus(store)
	}

	cfg, err := config.Load(getDataDir())
	if err != nil {
		return err
	}

	if ingestSync {
		return syncIssues(store, cfg)
	}

	if !ingestAll && ingestProject == "" {
		return fmt.Errorf("specify --project or --all")
	}

	if len(cfg.Projects) == 0 {
		return fmt.Errorf("no projects configured — add [[projects]] blocks to config.toml")
	}

	var toIngest []*config.ProjectConfig
	if ingestAll {
		for i := range cfg.Projects {
			toIngest = append(toIngest, &cfg.Projects[i])
		}
	} else {
		p := strings.ToUpper(ingestProject)
		proj := cfg.ProjectByKey(p)
		if proj == nil {
			keys := make([]string, 0, len(cfg.Projects))
			for _, pc := range cfg.Projects {
				keys = append(keys, pc.Key)
			}
			return fmt.Errorf("unknown project: %s (configured: %s)", ingestProject, strings.Join(keys, ", "))
		}
		toIngest = []*config.ProjectConfig{proj}
	}

	for _, proj := range toIngest {
		if err := ingestProject_(store, proj); err != nil {
			return err
		}
	}

	if err := normalizeIssueTypes(store, true, cfg); err != nil {
		fmt.Printf("Warning: type normalisation failed: %v\n", err)
	}
	return showIngestStatus(store)
}

func ingestProject_(store *storage.Store, proj *config.ProjectConfig) error {
	fmt.Printf("Ingesting %s (%s)...\n", proj.Key, proj.Name)

	count, err := jira.Count(proj.JQL)
	if err != nil {
		return fmt.Errorf("failed to count issues: %w", err)
	}
	fmt.Printf("  Found %d issues\n", count)

	limit := ingestLimit
	fmt.Printf("  Fetching issue list...")
	issues, err := jira.Search(proj.JQL, limit)
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

func syncIssues(store *storage.Store, cfg *config.Config) error {
	all, err := store.ListAllIssues()
	if err != nil {
		return err
	}

	var toSync []*storage.StoredIssue
	for _, issue := range all {
		if issue.Decision != storage.DecisionSkip {
			toSync = append(toSync, issue)
		}
	}

	fmt.Printf("Syncing %d issues (skipping already-skipped)...\n", len(toSync))

	updated, skipped, failed, reenriched := 0, 0, 0, 0
	for i, stored := range toSync {
		key := stored.Issue.Key
		fmt.Printf("\r  [%d/%d] %s", i+1, len(toSync), key)

		fresh, err := jira.GetIssue(key)
		if err != nil {
			fmt.Printf("\n  Warning: could not fetch %s: %v — auto-skipping\n", key, err)
			_ = store.UpdateDecision(key, storage.DecisionSkip, "sync: issue not found in Jira")
			skipped++
			failed++
			continue
		}

		// Check if the issue has moved to a terminal status.
		projectKey := strings.ToUpper(strings.SplitN(key, "-", 2)[0])
		if proj := cfg.ProjectByKey(projectKey); proj != nil {
			status := fresh.Status()
			for _, terminal := range proj.ExcludeStatuses {
				if strings.EqualFold(status, terminal) {
					_ = store.SaveIssue(fresh)
					_ = store.UpdateDecision(key, storage.DecisionSkip, fmt.Sprintf("sync: status=%s", status))
					skipped++
					goto next
				}
			}
		}

		// Still open — detect important field changes, then refresh Jira fields.
		{
			changedFields := jira.CompareIssueFields(stored.Issue, fresh)
			if err := store.SaveIssue(fresh); err != nil {
				fmt.Printf("\n  Warning: failed to save %s: %v\n", key, err)
				failed++
				goto next
			}
			updated++

			if len(changedFields) > 0 && stored.Enrichment != nil {
				reason := fmt.Sprintf("sync: fields changed: %s", strings.Join(changedFields, ", "))
				stored.Issue = fresh
				switch stored.Enrichment.Decision {
				case storage.EnrichAccepted:
					stored.Enrichment.Decision = storage.EnrichRejected
					stored.Enrichment.RejectionReason = reason
				case storage.EnrichRejected:
					if stored.Enrichment.RejectionReason != "" {
						stored.Enrichment.RejectionReason += "; " + reason
					} else {
						stored.Enrichment.RejectionReason = reason
					}
				default:
					stored.Enrichment = nil
				}
				if err := store.SaveStoredIssue(stored); err == nil {
					fmt.Printf("\n  %s: fields changed (%s), enrichment reset\n", key, strings.Join(changedFields, ", "))
					reenriched++
				}
			}
		}
	next:
	}

	fmt.Printf("\nSync complete: %d updated, %d auto-skipped, %d errors, %d enrichments reset\n",
		updated, skipped, failed, reenriched)
	if err := normalizeIssueTypes(store, true, cfg); err != nil {
		fmt.Printf("Warning: type normalisation failed: %v\n", err)
	}
	return showIngestStatus(store)
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
