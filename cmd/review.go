package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chzyer/readline"
	"github.com/fraud-zero/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	reviewDecision string
	reviewProject  string
	reviewAfter    int
)

var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Review and revise filter decisions interactively",
	Long:  `Iterate over previously decided issues (keep/skip/defer) and revise their decisions.`,
	RunE:  runReview,
}

func init() {
	rootCmd.AddCommand(reviewCmd)
	reviewCmd.Flags().StringVar(&reviewDecision, "decision", "keep,skip,defer", "comma-separated decisions to include (keep,skip,defer)")
	reviewCmd.Flags().StringVar(&reviewProject, "project", "", "filter by project key (e.g. F0)")
	reviewCmd.Flags().IntVar(&reviewAfter, "after", 0, "start after this issue number (1-based)")
}

func runReview(cmd *cobra.Command, args []string) error {
	tokens := strings.Split(reviewDecision, ",")
	decisions := make([]storage.FilterDecision, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(strings.ToLower(t))
		switch storage.FilterDecision(t) {
		case storage.DecisionKeep, storage.DecisionSkip, storage.DecisionDefer:
			decisions = append(decisions, storage.FilterDecision(t))
		default:
			return fmt.Errorf("unknown decision %q: must be keep, skip, or defer", t)
		}
	}
	if len(decisions) == 0 {
		return fmt.Errorf("--decision cannot be empty")
	}

	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	issues, err := store.GetIssuesByDecision(decisions)
	if err != nil {
		return err
	}

	if reviewProject != "" {
		filtered := issues[:0]
		for _, issue := range issues {
			if strings.EqualFold(issue.Issue.Project(), reviewProject) {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}

	if len(issues) == 0 {
		fmt.Println("No issues match the given criteria.")
		return nil
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Issue.Project() != issues[j].Issue.Project() {
			return issues[i].Issue.Project() < issues[j].Issue.Project()
		}
		return issues[i].Issue.Key < issues[j].Issue.Key
	})

	start := 0
	if reviewAfter > 0 {
		if reviewAfter >= len(issues) {
			fmt.Printf("--after %d is past the end (%d issues total)\n", reviewAfter, len(issues))
			return nil
		}
		start = reviewAfter
		fmt.Printf("Skipping to issue %d/%d.\n", start+1, len(issues))
	}

	fmt.Printf("Found %d issues. Starting review...\n", len(issues))
	fmt.Println("Commands: [k]eep [s]kip [d]efer [p]ending [n]ext [v]iew [o]pen [q]uit [?]help")
	fmt.Println()

	rl, err := readline.New("")
	if err != nil {
		return fmt.Errorf("readline: %w", err)
	}
	defer rl.Close()

	changed := 0

	for i := start; i < len(issues); {
		issue := issues[i]
		displayReviewSummary(issue, i+1, len(issues))

		rl.SetPrompt("\nAction: ")
		input, err := rl.Readline()
		if err != nil {
			return err
		}
		input = strings.TrimSpace(strings.ToLower(input))

		switch input {
		case "k", "keep":
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionKeep, ""); err != nil {
				return err
			}
			changed++
			i++
			fmt.Println("✓ Kept")

		case "s", "skip":
			rl.SetPrompt("Reason (optional): ")
			reason, _ := rl.Readline()
			reason = strings.TrimSpace(reason)
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return err
			}
			changed++
			i++
			fmt.Println("✗ Skipped")

		case "d", "defer":
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionDefer, ""); err != nil {
				return err
			}
			changed++
			i++
			fmt.Println("⏸ Deferred")

		case "p", "pending":
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionPending, ""); err != nil {
				return err
			}
			changed++
			i++
			fmt.Println("↩ Reset to pending")

		case "n", "next":
			i++

		case "v", "view":
			displayIssueFull(issue)

		case "o", "open":
			openInBrowser(issue.Issue.Key)

		case "q", "quit":
			fmt.Printf("\nSession ended. %d issue(s) changed.\n", changed)
			return nil

		case "?", "help":
			printReviewHelp()

		default:
			fmt.Println("Unknown command. Type ? for help.")
		}

		fmt.Println()
	}

	fmt.Printf("\nAll issues reviewed. %d issue(s) changed.\n", changed)
	return nil
}

func displayReviewSummary(issue *storage.StoredIssue, current, total int) {
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	if e := issue.Enrichment; e != nil {
		fmt.Printf("[%d/%d] %s • %s  [enriched: %s by %s]\n", current, total, issue.Issue.Key, issue.Decision, e.Confidence, e.EnrichedBy)
	} else {
		fmt.Printf("[%d/%d] %s • %s\n", current, total, issue.Issue.Key, issue.Decision)
	}
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("Summary:  %s\n", issue.Issue.Summary())
	fmt.Printf("Type:     %s | Status: %s | Priority: %s\n",
		issue.Issue.IssueType(), issue.Issue.Status(), issue.Issue.Priority())
	fmt.Printf("Reporter: %s | Assignee: %s\n",
		nvl(issue.Issue.Reporter(), "-"), nvl(issue.Issue.Assignee(), "-"))
	fmt.Printf("Created:  %s\n", formatTime(issue.Issue.Created()))

	if issue.Reason != "" {
		fmt.Printf("Reason:   %s\n", issue.Reason)
	}

	if labels := issue.Issue.Labels(); len(labels) > 0 {
		fmt.Printf("Labels:   %s\n", strings.Join(labels, ", "))
	}

	if issue.Enrichment == nil {
		if desc := issue.Issue.Description(); desc != "" {
			fmt.Printf("\nDescription:\n%s\n", truncateLines(desc, 5))
		}
		return
	}

	e := issue.Enrichment

	// Title
	oldTitle := issue.Issue.Summary()
	if e.ProposedTitle != "" && e.ProposedTitle != oldTitle {
		fmt.Println("\nProposed title:")
		fmt.Printf("  \033[31m- %s\033[0m\n", oldTitle)
		fmt.Printf("  \033[32m+ %s\033[0m\n", e.ProposedTitle)
	}

	// Labels
	var oldLabels []string
	if issue.Classification != nil && len(issue.Classification.Labels) > 0 {
		oldLabels = issue.Classification.Labels
	} else {
		oldLabels = issue.Issue.Labels()
	}
	if len(e.ProposedLabels) > 0 {
		oldStr := "(none)"
		if len(oldLabels) > 0 {
			oldStr = strings.Join(oldLabels, ", ")
		}
		newStr := strings.Join(e.ProposedLabels, ", ")
		if oldStr != newStr {
			fmt.Println("\nProposed labels:")
			fmt.Printf("  \033[31m- %s\033[0m\n", oldStr)
			fmt.Printf("  \033[32m+ %s\033[0m\n", newStr)
		}
	}

	// Description
	oldDesc := strings.TrimSpace(issue.Issue.Description())
	newDesc := strings.TrimSpace(e.ProposedDescription)
	if newDesc != "" && newDesc != oldDesc {
		fmt.Println("\nProposed description:")
		if oldDesc != "" {
			fmt.Printf("  \033[31m--- original (%d chars) ---\033[0m\n", len(oldDesc))
			for _, line := range strings.Split(oldDesc, "\n") {
				fmt.Printf("  \033[31m  %s\033[0m\n", line)
			}
		} else {
			fmt.Printf("  \033[31m  (empty)\033[0m\n")
		}
		fmt.Printf("  \033[32m--- proposed (%d chars) ---\033[0m\n", len(newDesc))
		for _, line := range strings.Split(newDesc, "\n") {
			fmt.Printf("  \033[32m  %s\033[0m\n", line)
		}
	}

	// Related files
	if len(e.RelatedFiles) > 0 {
		fmt.Println("\nRelated files:")
		for _, f := range e.RelatedFiles {
			fmt.Printf("  %s\n", f)
		}
	}

	// Agent notes
	if e.Notes != "" {
		fmt.Println("\nAgent notes:")
		for _, line := range strings.Split(e.Notes, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
}

func printReviewHelp() {
	fmt.Println(`
Commands:
  k, keep    - Mark as keep
  s, skip    - Mark as skip (prompts for reason)
  d, defer   - Mark as defer
  p, pending - Reset to pending (re-enters filter queue)
  n, next    - Advance without changing decision
  v, view    - Show full issue details
  o, open    - Open issue in browser
  q, quit    - Save and quit
  ?, help    - Show this help
`)
}
