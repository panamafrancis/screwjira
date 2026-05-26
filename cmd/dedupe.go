package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chzyer/readline"
	"github.com/panamafrancis/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	dedupeThreshold float64
	dedupeProject   string
)

var dedupeCmd = &cobra.Command{
	Use:   "dedupe",
	Short: "Find and interactively resolve duplicate issues",
	Long: `Compares all kept issues by title similarity and presents candidate duplicate
pairs for review. Use --threshold to adjust sensitivity (default 0.6).`,
	RunE: runDedupe,
}

func init() {
	rootCmd.AddCommand(dedupeCmd)
	dedupeCmd.Flags().Float64Var(&dedupeThreshold, "threshold", 0.6, "minimum title similarity to flag as duplicate (0–1)")
	dedupeCmd.Flags().StringVar(&dedupeProject, "project", "", "limit scan to a single project (e.g. F0)")
}

type dupePair struct {
	A, B       *storage.StoredIssue
	Score      float64 // max(TitleScore, DescScore)
	TitleScore float64
	DescScore  float64
}

func enrichedDescription(issue *storage.StoredIssue) string {
	if issue.Enrichment != nil && issue.Enrichment.ProposedDescription != "" {
		return issue.Enrichment.ProposedDescription
	}
	return issue.Issue.Description()
}

func runDedupe(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	if dedupeProject != "" {
		filtered := issues[:0]
		for _, issue := range issues {
			if strings.EqualFold(issue.Issue.Project(), dedupeProject) {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}

	if len(issues) == 0 {
		fmt.Println("No kept issues to scan.")
		return nil
	}

	fmt.Printf("Scanning %d issues for duplicates (threshold: %.0f%%)...\n", len(issues), dedupeThreshold*100)

	var pairs []dupePair
	for i := 0; i < len(issues); i++ {
		for j := i + 1; j < len(issues); j++ {
			titleScore := wordOverlapScore(issues[i].Issue.Summary(), issues[j].Issue.Summary())
			descScore := wordOverlapScore(enrichedDescription(issues[i]), enrichedDescription(issues[j]))
			score := titleScore
			if descScore > score {
				score = descScore
			}
			if score >= dedupeThreshold {
				pairs = append(pairs, dupePair{issues[i], issues[j], score, titleScore, descScore})
			}
		}
	}

	if len(pairs) == 0 {
		fmt.Printf("No duplicate candidates found.\n")
		return nil
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].Score > pairs[j].Score
	})

	fmt.Printf("Found %d candidate pairs.\n", len(pairs))
	fmt.Println("Commands: [1] keep first  [2] keep second  [m1]/[m2] merge into first/second  [b]oth  [n]ext  [o]pen  [q]uit")
	fmt.Println()

	rl, err := readline.New("")
	if err != nil {
		return fmt.Errorf("readline: %w", err)
	}
	defer rl.Close()

	resolved := 0
	for i := 0; i < len(pairs); {
		pair := pairs[i]

		// Reload from disk — either may have been skipped earlier in this session.
		a, errA := store.LoadIssue(pair.A.Issue.Key)
		b, errB := store.LoadIssue(pair.B.Issue.Key)
		if errA != nil || errB != nil || a.Decision == storage.DecisionSkip || b.Decision == storage.DecisionSkip {
			i++
			continue
		}

		displayDupePair(pair, i+1, len(pairs))

		rl.SetPrompt("\nAction: ")
		input, err := rl.Readline()
		if err != nil {
			return err
		}
		input = strings.TrimSpace(strings.ToLower(input))

		switch input {
		case "1":
			reason := fmt.Sprintf("duplicate of %s", pair.A.Issue.Key)
			if err := store.UpdateDecision(pair.B.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return err
			}
			resolved++
			i++
			fmt.Printf("✓ Kept %s, skipped %s\n", pair.A.Issue.Key, pair.B.Issue.Key)

		case "2":
			reason := fmt.Sprintf("duplicate of %s", pair.B.Issue.Key)
			if err := store.UpdateDecision(pair.A.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return err
			}
			resolved++
			i++
			fmt.Printf("✓ Kept %s, skipped %s\n", pair.B.Issue.Key, pair.A.Issue.Key)

		case "m1", "m2":
			var primary, secondary *storage.StoredIssue
			if input == "m1" {
				primary, secondary = a, b
			} else {
				primary, secondary = b, a
			}
			if err := mergeInto(store, primary, secondary); err != nil {
				return err
			}
			resolved++
			i++
			fmt.Printf("✓ Merged %s into %s — queued for re-enrichment\n", secondary.Issue.Key, primary.Issue.Key)

		case "b", "both":
			i++
			fmt.Println("Both kept.")

		case "n", "next":
			i++

		case "o", "open":
			fmt.Printf("Opening %s and %s in browser...\n", pair.A.Issue.Key, pair.B.Issue.Key)
			openInBrowser(pair.A.Issue.Key)
			openInBrowser(pair.B.Issue.Key)
			continue

		case "q", "quit":
			fmt.Printf("\nSession ended. %d pairs resolved.\n", resolved)
			return nil

		default:
			fmt.Println("Unknown command. Use 1, 2, m1, m2, b, n, o, or q.")
		}

		fmt.Println()
	}

	fmt.Printf("\nDone! %d pairs resolved.\n", resolved)
	return nil
}

func displayDupePair(pair dupePair, current, total int) {
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("[%d/%d] title: %.0f%%  desc: %.0f%%\n", current, total, pair.TitleScore*100, pair.DescScore*100)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	printDupeIssue("[1]", pair.A)
	fmt.Println()
	printDupeIssue("[2]", pair.B)
}

// mergeInto appends secondary's content to primary's enrichment instructions and
// queues the primary for re-enrichment. The secondary is skipped.
func mergeInto(store *storage.Store, primary, secondary *storage.StoredIssue) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Merged with %s. Please incorporate both issues into one enriched description.\n", secondary.Issue.Key))
	sb.WriteString(fmt.Sprintf("\nMerged issue title: %s\n", secondary.Issue.Summary()))
	if desc := enrichedDescription(secondary); desc != "" {
		sb.WriteString("\nMerged issue description:\n")
		if len(desc) > 1500 {
			sb.WriteString(desc[:1500])
			sb.WriteString("\n...(truncated)")
		} else {
			sb.WriteString(desc)
		}
	}

	if primary.Enrichment == nil {
		primary.Enrichment = &storage.Enrichment{}
	}
	primary.Enrichment.Decision = storage.EnrichRejected
	if primary.Enrichment.RejectionReason != "" {
		primary.Enrichment.RejectionReason += "\n\n" + sb.String()
	} else {
		primary.Enrichment.RejectionReason = sb.String()
	}
	if err := store.SaveStoredIssue(primary); err != nil {
		return fmt.Errorf("failed to save %s: %w", primary.Issue.Key, err)
	}

	reason := fmt.Sprintf("merged into %s", primary.Issue.Key)
	return store.UpdateDecision(secondary.Issue.Key, storage.DecisionSkip, reason)
}

func printDupeIssue(tag string, issue *storage.StoredIssue) {
	typeStr := issue.Issue.IssueType()
	if issue.Classification != nil && issue.Classification.Type != "" {
		typeStr = issue.Classification.Type
	}
	fmt.Printf("%s %s  (%s / %s)  %s\n", tag, issue.Issue.Key, issue.Issue.Project(), typeStr, issue.Issue.Summary())
	if desc := enrichedDescription(issue); desc != "" {
		fmt.Printf("    %s\n", truncateLines(desc, 2))
	}
}
