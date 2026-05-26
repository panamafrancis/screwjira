package cmd

import (
	"fmt"
	"strings"

	"github.com/panamafrancis/screwjira/internal/config"
	"github.com/panamafrancis/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	normalizeApply        bool
	normalizeCollapseEpics bool
)

var normalizeCmd = &cobra.Command{
	Use:   "normalize",
	Short: "Normalize issue types to bug/task/feature",
	Long: `Map Jira issue types to three GitHub output types: bug, task, feature.
Runs as a dry-run by default; use --apply to write changes.

Use --collapse-epics to auto-skip child issues (configured via collapse_child_type)
that are direct children of a kept parent issue (configured via light_mode_type).
Those issues will be summarised inside the parent's enrichment instead of becoming
separate GitHub issues.`,
	RunE: runNormalizeCmd,
}

func init() {
	rootCmd.AddCommand(normalizeCmd)
	normalizeCmd.Flags().BoolVar(&normalizeApply, "apply", false, "write changes to storage (default: dry run)")
	normalizeCmd.Flags().BoolVar(&normalizeCollapseEpics, "collapse-epics", false, "skip DISC Epics that are children of a kept DISC Idea")
}

func runNormalizeCmd(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}
	cfg, err := config.Load(getDataDir())
	if err != nil {
		return err
	}
	if err := normalizeIssueTypes(store, normalizeApply, cfg); err != nil {
		return err
	}
	if normalizeCollapseEpics {
		fmt.Println()
		return collapseChildEpics(store, normalizeApply, cfg)
	}
	return nil
}

// normalizeIssueType maps a Jira issue type to one of bug/task/feature.
func normalizeIssueType(jiraType string) string {
	switch strings.ToLower(jiraType) {
	case "bug":
		return "bug"
	case "epic", "idea", "new feature":
		return "feature"
	default:
		return "task"
	}
}

// collapseChildEpics finds kept child issues (collapse_child_type) whose parent
// is a kept parent issue (light_mode_type) and skips them. Those child issues
// are summarised inside the parent's enrichment instead of becoming separate GitHub issues.
func collapseChildEpics(store *storage.Store, apply bool, cfg *config.Config) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	// Index kept parent-type issue keys → the project key they belong to.
	parentProject := make(map[string]string) // issue key → project key
	for _, issue := range issues {
		proj := cfg.ProjectByKey(issue.Issue.Project())
		if proj == nil || proj.LightModeType == "" {
			continue
		}
		if strings.EqualFold(issue.Issue.IssueType(), proj.LightModeType) {
			parentProject[issue.Issue.Key] = proj.Key
		}
	}

	var toSkip []*storage.StoredIssue
	for _, issue := range issues {
		proj := cfg.ProjectByKey(issue.Issue.Project())
		if proj == nil || proj.CollapseChildType == "" {
			continue
		}
		parentProjKey, ok := parentProject[issue.Issue.ParentKey()]
		if !ok || parentProjKey != proj.Key {
			continue
		}
		if strings.EqualFold(issue.Issue.IssueType(), proj.CollapseChildType) {
			toSkip = append(toSkip, issue)
		}
	}

	label := "Collapse child issues"
	if !apply {
		label += " (dry run)"
	}
	fmt.Printf("%s:\n", label)

	if len(toSkip) == 0 {
		fmt.Println("  nothing to collapse — no collapse_child_type configured or no matching issues found")
		return nil
	}

	for _, issue := range toSkip {
		fmt.Printf("  %s ← %s  %s\n", issue.Issue.ParentKey(), issue.Issue.Key, truncate(issue.Issue.Summary(), 60))
		if apply {
			reason := fmt.Sprintf("child epic of %s — summarised in parent enrichment", issue.Issue.ParentKey())
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return fmt.Errorf("failed to skip %s: %w", issue.Issue.Key, err)
			}
		}
	}

	if apply {
		fmt.Printf("  skipped %d issues\n", len(toSkip))
	} else {
		fmt.Printf("\n  %d issues would be skipped. Run with --apply to skip them.\n", len(toSkip))
	}
	return nil
}

// normalizeIssueTypes iterates all kept issues and sets Classification.Type.
// When apply is false it prints a dry-run summary without writing anything.
func normalizeIssueTypes(store *storage.Store, apply bool, cfg *config.Config) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	counts := map[string]int{"feature": 0, "bug": 0, "task": 0}
	alreadySet := 0
	changed := 0

	for _, issue := range issues {
		desired := normalizeIssueType(issue.Issue.IssueType())
		counts[desired]++

		if issue.Classification != nil && issue.Classification.Type == desired {
			alreadySet++
			continue
		}

		if !apply {
			continue
		}

		if issue.Classification == nil {
			issue.Classification = &storage.Classification{}
		}
		issue.Classification.Type = desired

		// Issues of the light_mode_type inherit their Jira labels into Classification.
		if proj := cfg.ProjectByKey(issue.Issue.Project()); proj != nil && proj.LightModeType != "" {
			if strings.EqualFold(issue.Issue.IssueType(), proj.LightModeType) {
				if len(issue.Classification.Labels) == 0 {
					issue.Classification.Labels = issue.Issue.Labels()
				}
			}
		}

		if err := store.SaveStoredIssue(issue); err != nil {
			return fmt.Errorf("failed to save %s: %w", issue.Issue.Key, err)
		}
		changed++
	}

	label := "Issue type normalisation"
	if !apply {
		label += " (dry run)"
	}
	fmt.Printf("%s:\n", label)
	fmt.Printf("  feature: %d\n", counts["feature"])
	fmt.Printf("  bug:     %d\n", counts["bug"])
	fmt.Printf("  task:    %d\n", counts["task"])

	if apply {
		fmt.Printf("  updated: %d, already correct: %d\n", changed, alreadySet)
	} else {
		fmt.Printf("  already set: %d\n", alreadySet)
		fmt.Println("\nRun with --apply to write changes.")
	}
	return nil
}
